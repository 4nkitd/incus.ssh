package controlapi

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"incus-for-devs/internal/core"
)

func (s service) mcpHandler(version string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "incus-for-devs", Version: version}, nil)
	addTool(server, "list_vms", "List owned Incus containers, not hardware VMs.", true,
		func(ctx context.Context, p core.Principal, _ struct{}) (any, error) { return s.list(ctx, p) })
	addTool(server, "list_images", "List administrator-cached images available for container creation.", true,
		func(ctx context.Context, p core.Principal, _ struct{}) (any, error) { return s.images(ctx, p) })
	addTool(server, "create_vm", "Create an owned container within operator resource limits. Omitted fields use backend defaults.", false, s.create)
	for _, action := range []string{"start", "stop", "restart", "delete"} {
		description := action + " an owned container. Stop and restart interrupt running processes. Delete permanently destroys its data. Obtain user approval before destructive actions."
		addTool(server, action+"_vm", description, false,
			func(ctx context.Context, p core.Principal, in NameInput) (any, error) {
				return s.lifecycle(ctx, p, action, in.Name)
			})
	}
	addTool(server, "exec_vm", "Run a noninteractive shell command as guest root inside an owned container. Can modify or delete guest data and access its network. Output is bounded to 1 MiB per stream; requests have the operator's timeout. Obtain approval for destructive commands.", false, s.exec)
	if _, ok := s.backend.(core.HostingBackend); ok {
		addTool(server, "hosting_status", "Show image-hosting capabilities, operator limits and configuration requirements.", true, func(ctx context.Context, p core.Principal, _ struct{}) (any, error) { return s.system(ctx, p) })
		addTool(server, "create_app", "Deploy an image from Docker Hub or an operator-approved public HTTPS OCI registry as an unprivileged Incus application container. hosting_status lists available registry hosts and optional quick picks. No archive downloads, Docker Compose, host mounts or privileged mode. Data persists across stop/start but deletion removes it. App starts private.", false, s.createApp)
		addTool(server, "app_logs", "Read the last bounded console output of an owned container. Logs can contain application secrets.", true, func(ctx context.Context, p core.Principal, in NameInput) (any, error) {
			return s.appLogs(ctx, p, in.Name)
		})
		addTool(server, "publish_app", "Expose an app's HTTP port at its configured application hostname. Obtain explicit user approval before publishing. Port zero makes it private. Restart requires publishing again.", false, func(ctx context.Context, p core.Principal, in struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}) (any, error) {
			return s.publishApp(ctx, p, in.Name, in.Port)
		})
	}
	if _, ok := s.backend.(core.AppInspector); ok {
		addTool(server, "app_details", "Read runtime counters, configured limits and environment variable names of an owned container. Environment values are never returned.", true, func(ctx context.Context, p core.Principal, in NameInput) (any, error) {
			return s.inspectApp(ctx, p, in.Name)
		})
		addTool(server, "set_app_environment", "Change environment values on an owned app. A null value removes a variable. Values are not logged. Does not restart the app; explicitly request approval before restarting to apply changes to its main process.", false, func(ctx context.Context, p core.Principal, in struct {
			Name   string             `json:"name"`
			Values map[string]*string `json:"values"`
		}) (any, error) {
			return s.updateEnvironment(ctx, p, in.Name, core.EnvironmentUpdate{Values: in.Values})
		})
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxBody,
		// The outer middleware enforces the exact public Host and Origin behind Caddy.
		DisableLocalhostProtection: true, PropagateRequestCancellation: true,
	})
}

func addTool[In any](server *mcp.Server, name, description string, readOnly bool, call func(context.Context, core.Principal, In) (any, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			identity, ok := ctx.Value(identityKey{}).(requestIdentity)
			if !ok {
				return nil, nil, badInput("authenticated request context required")
			}
			// Older MCP versions detach request cancellation; retain our authenticated request deadline and token revocation.
			operationCtx, cancel := context.WithCancel(identity.ctx)
			defer cancel()
			stop := context.AfterFunc(ctx, cancel)
			defer stop()
			if err := operationCtx.Err(); err != nil {
				return nil, nil, err
			}
			result, err := call(operationCtx, identity.principal, in)
			return nil, result, err
		})
}
