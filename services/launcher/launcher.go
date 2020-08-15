/*
  Launches new collection against clients.
*/

package launcher

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	errors "github.com/pkg/errors"
	actions_proto "www.velocidex.com/golang/velociraptor/actions/proto"
	api_proto "www.velocidex.com/golang/velociraptor/api/proto"
	"www.velocidex.com/golang/velociraptor/artifacts"
	artifacts_proto "www.velocidex.com/golang/velociraptor/artifacts/proto"
	config_proto "www.velocidex.com/golang/velociraptor/config/proto"
	"www.velocidex.com/golang/velociraptor/constants"
	crypto_proto "www.velocidex.com/golang/velociraptor/crypto/proto"
	"www.velocidex.com/golang/velociraptor/datastore"
	flows_proto "www.velocidex.com/golang/velociraptor/flows/proto"
	"www.velocidex.com/golang/velociraptor/logging"
	"www.velocidex.com/golang/velociraptor/paths"
	"www.velocidex.com/golang/velociraptor/services"
)

type Launcher struct{}

func (self *Launcher) CompileCollectorArgs(
	ctx context.Context,
	config_obj *config_proto.Config,
	principal string,
	repository *artifacts.Repository,
	collector_request *flows_proto.ArtifactCollectorArgs) (
	*actions_proto.VQLCollectorArgs, error) {

	// Update the flow's artifacts list.
	vql_collector_args := &actions_proto.VQLCollectorArgs{
		OpsPerSecond: collector_request.OpsPerSecond,
		Timeout:      collector_request.Timeout,
		MaxRow:       1000,
	}

	// All artifacts are compiled to the same client request
	// because they all run serially.
	for _, name := range collector_request.Artifacts {
		var artifact *artifacts_proto.Artifact = nil
		if collector_request.AllowCustomOverrides {
			artifact, _ = repository.Get("Custom." + name)
		}

		if artifact == nil {
			artifact, _ = repository.Get(name)
		}

		if artifact == nil {
			return nil, errors.New("Unknown artifact " + name)
		}

		err := repository.CheckAccess(config_obj, artifact, principal)
		if err != nil {
			return nil, err
		}

		err = repository.Compile(artifact, vql_collector_args)
		if err != nil {
			return nil, err
		}

		err = self.EnsureToolsDeclared(ctx, config_obj, artifact)
		if err != nil {
			return nil, err
		}
	}

	// Add any artifact dependencies.
	err := repository.PopulateArtifactsVQLCollectorArgs(vql_collector_args)
	if err != nil {
		return nil, err
	}

	err = self.AddArtifactCollectorArgs(
		config_obj, vql_collector_args, collector_request)
	if err != nil {
		return nil, err
	}

	err = getDependentTools(ctx, config_obj, vql_collector_args)
	if err != nil {
		return nil, err
	}

	err = artifacts.Obfuscate(config_obj, vql_collector_args)
	return vql_collector_args, err
}

func getDependentTools(
	ctx context.Context,
	config_obj *config_proto.Config,
	vql_collector_args *actions_proto.VQLCollectorArgs) error {

	logger := logging.GetLogger(config_obj, &logging.FrontendComponent)
	for _, tool := range vql_collector_args.Tools {
		err := AddToolDependency(ctx, config_obj, tool, vql_collector_args)
		if err != nil {
			logger.Error("While Adding dependencies: ", err)
			return err
		}
	}

	return nil
}

// Make sure we know about tools the artifact itself defines.
func (self *Launcher) EnsureToolsDeclared(
	ctx context.Context, config_obj *config_proto.Config,
	artifact *artifacts_proto.Artifact) error {
	logger := logging.GetLogger(config_obj, &logging.FrontendComponent)
	for _, tool := range artifact.Tools {
		_, err := services.GetInventory().GetToolInfo(ctx, config_obj, tool.Name)
		if err != nil {
			// Add tool info if it is not known but do not
			// override existing tool. This allows the
			// admin to override tools from the artifact
			// itself.
			logger.Info("Adding tool %v from artifact %v",
				tool.Name, artifact.Name)
			err = services.GetInventory().AddTool(ctx, config_obj, tool)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func AddToolDependency(
	ctx context.Context,
	config_obj *config_proto.Config,
	tool string, vql_collector_args *actions_proto.VQLCollectorArgs) error {
	tool_info, err := services.GetInventory().GetToolInfo(ctx, config_obj, tool)
	if err != nil {
		return err
	}

	vql_collector_args.Env = append(vql_collector_args.Env, &actions_proto.VQLEnv{
		Key:   fmt.Sprintf("Tool_%v_HASH", tool_info.Name),
		Value: tool_info.Hash,
	})

	vql_collector_args.Env = append(vql_collector_args.Env, &actions_proto.VQLEnv{
		Key:   fmt.Sprintf("Tool_%v_FILENAME", tool_info.Name),
		Value: tool_info.Filename,
	})

	if len(config_obj.Client.ServerUrls) == 0 {
		return errors.New("No server URLs configured!")
	}

	// Where to download the binary from.
	url := config_obj.Client.ServerUrls[0] + "public/" + tool_info.FilestorePath

	// If we dont want to serve the binary locally, just
	// tell the client where to get it from.
	if !tool_info.ServeLocally && tool_info.Url != "" {
		url = tool_info.Url
	}
	vql_collector_args.Env = append(vql_collector_args.Env, &actions_proto.VQLEnv{
		Key:   fmt.Sprintf("Tool_%v_URL", tool_info.Name),
		Value: url,
	})
	return nil
}

func (self *Launcher) ScheduleArtifactCollection(
	ctx context.Context,
	config_obj *config_proto.Config,
	principal string,
	repository *artifacts.Repository,
	collector_request *flows_proto.ArtifactCollectorArgs) (string, error) {

	args := collector_request.CompiledCollectorArgs
	if args == nil {
		// Compile and cache the compilation for next time
		// just in case this request is reused.

		// NOTE: We assume that compiling the artifact is a
		// pure function so caching is appropriate.
		compiled, err := self.CompileCollectorArgs(
			ctx, config_obj, principal, repository, collector_request)
		if err != nil {
			return "", err
		}
		args = append(args, compiled)
	}

	return ScheduleArtifactCollectionFromCollectorArgs(
		config_obj, collector_request, args)
}

func ScheduleArtifactCollectionFromCollectorArgs(
	config_obj *config_proto.Config,
	collector_request *flows_proto.ArtifactCollectorArgs,
	vql_collector_args []*actions_proto.VQLCollectorArgs) (string, error) {

	client_id := collector_request.ClientId
	if client_id == "" {
		return "", errors.New("Client id not provided.")
	}

	// Generate a new collection context.
	collection_context := &flows_proto.ArtifactCollectorContext{
		SessionId:  NewFlowId(client_id),
		CreateTime: uint64(time.Now().UnixNano() / 1000),
		State:      flows_proto.ArtifactCollectorContext_RUNNING,
		Request:    collector_request,
		ClientId:   client_id,
	}
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return "", err
	}

	// Save the collection context.
	flow_path_manager := paths.NewFlowPathManager(client_id,
		collection_context.SessionId)
	err = db.SetSubject(config_obj,
		flow_path_manager.Path(),
		collection_context)
	if err != nil {
		return "", err
	}

	tasks := []*crypto_proto.GrrMessage{}

	for _, arg := range vql_collector_args {
		// The task we will schedule for the client.
		task := &crypto_proto.GrrMessage{
			SessionId:       collection_context.SessionId,
			RequestId:       constants.ProcessVQLResponses,
			VQLClientAction: arg}

		// Send an urgent request to the client.
		if collector_request.Urgent {
			task.Urgent = true
		}

		err = db.QueueMessageForClient(
			config_obj, client_id, task)
		if err != nil {
			return "", err
		}
		tasks = append(tasks, task)
	}

	// Record the tasks for provenance of what we actually did.
	err = db.SetSubject(config_obj,
		flow_path_manager.Task().Path(),
		&api_proto.ApiFlowRequestDetails{Items: tasks})
	if err != nil {
		return "", err
	}

	return collection_context.SessionId, nil
}

// Adds any parameters set in the ArtifactCollectorArgs into the
// VQLCollectorArgs.
func (self *Launcher) AddArtifactCollectorArgs(
	config_obj *config_proto.Config,
	vql_collector_args *actions_proto.VQLCollectorArgs,
	collector_request *flows_proto.ArtifactCollectorArgs) error {

	// Add any Environment Parameters from the request.
	if collector_request.Parameters == nil {
		return nil
	}

	// We can only specify a parameter which is defined already
	for _, item := range collector_request.Parameters.Env {
		addOrReplaceParameter(item, vql_collector_args.Env)
	}

	return nil
}

// We do not expect too many parameters so linear search is appropriate.
func addOrReplaceParameter(
	param *actions_proto.VQLEnv, env []*actions_proto.VQLEnv) {

	// Try to replace it if it is already there.
	for _, item := range env {
		if item.Key == param.Key {
			item.Value = param.Value
			return
		}
	}
	env = append(env, param)
}

func (self *Launcher) SetFlowIdForTests(id string) {
	NextFlowIdForTests = id
}

var (
	NextFlowIdForTests string
)

func NewFlowId(client_id string) string {
	if NextFlowIdForTests != "" {
		result := NextFlowIdForTests
		NextFlowIdForTests = ""
		return result
	}

	buf := make([]byte, 8)
	rand.Read(buf)

	binary.BigEndian.PutUint32(buf, uint32(time.Now().Unix()))
	result := base32.HexEncoding.EncodeToString(buf)[:13]

	return constants.FLOW_PREFIX + result
}

func StartLauncherService(
	ctx context.Context,
	wg *sync.WaitGroup,
	config_obj *config_proto.Config) error {

	services.RegisterLauncher(&Launcher{})
	return nil
}
