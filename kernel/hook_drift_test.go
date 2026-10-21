package kernel_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/internal/schemadrift"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
)

// hookIO is the Go type of each kernel point's JSON input and output.
var hookIO = map[string][2]reflect.Type{
	"session_start":       {reflect.TypeFor[kernel.SessionStartInput](), reflect.TypeFor[kernel.SessionStartOutput]()},
	"session_end":         {reflect.TypeFor[kernel.SessionEndInput](), reflect.TypeFor[kernel.SessionEndOutput]()},
	"user_prompt_submit":  {reflect.TypeFor[kernel.UserPromptSubmitInput](), reflect.TypeFor[kernel.UserPromptSubmitOutput]()},
	"command_invoke":      {reflect.TypeFor[kernel.CommandInvokeInput](), reflect.TypeFor[kernel.CommandInvokeOutput]()},
	"pre_model_request":   {reflect.TypeFor[kernel.PreModelRequestInput](), reflect.TypeFor[kernel.PreModelRequestOutput]()},
	"post_model_response": {reflect.TypeFor[kernel.PostModelResponseInput](), reflect.TypeFor[kernel.PostModelResponseOutput]()},
	"pre_tool_use":        {reflect.TypeFor[kernel.PreToolUseInput](), reflect.TypeFor[kernel.PreToolUseOutput]()},
	"approval":            {reflect.TypeFor[kernel.ApprovalRequest](), reflect.TypeFor[kernel.ApprovalDecision]()},
	"post_tool_use":       {reflect.TypeFor[kernel.PostToolUseInput](), reflect.TypeFor[kernel.PostToolUseOutput]()},
	"post_tool_batch":     {reflect.TypeFor[kernel.PostToolBatchInput](), reflect.TypeFor[kernel.PostToolBatchOutput]()},
	"stop":                {reflect.TypeFor[kernel.StopInput](), reflect.TypeFor[kernel.StopOutput]()},
	"provider_switch":     {reflect.TypeFor[kernel.ProviderSwitchInput](), reflect.TypeFor[kernel.ProviderSwitchOutput]()},
	"turn_end":            {reflect.TypeFor[kernel.TurnEndInput](), reflect.TypeFor[kernel.TurnEndOutput]()},
	"pre_compact":         {reflect.TypeFor[kernel.PreCompactInput](), reflect.TypeFor[kernel.PreCompactOutput]()},
	"post_compact":        {reflect.TypeFor[kernel.PostCompactInput](), reflect.TypeFor[kernel.PostCompactOutput]()},
	"task_start":          {reflect.TypeFor[kernel.TaskStartInput](), reflect.TypeFor[kernel.TaskStartOutput]()},
	"task_notify":         {reflect.TypeFor[kernel.TaskNotifyInput](), reflect.TypeFor[kernel.TaskNotifyOutput]()},
}

func TestHookSchemaDrift(t *testing.T) {
	schemadrift.Extra(reflect.TypeFor[kernel.ToolSpec](), "timeout_ms")
	schemadrift.Extra(reflect.TypeFor[kernel.TurnResult](), "error")
	schemadrift.Extra(reflect.TypeFor[message.ToolCallBlock](), "type")
	schemadrift.Extra(reflect.TypeFor[message.ToolResultBlock](), "type")
	var points []string
	for _, p := range kernel.HookPoints() {
		points = append(points, p.Name())
		io, ok := hookIO[p.Name()]
		if !ok {
			t.Errorf("no Go types listed for point %s", p.Name())
			continue
		}
		schemadrift.Check(t, io[0], "hook.schema.json#/$defs/"+p.Name()+"_input")
		schemadrift.Check(t, io[1], "hook.schema.json#/$defs/"+p.Name()+"_output")
	}
	schemadrift.Check(t, reflect.TypeFor[kernel.Injection](), "hook.schema.json#/$defs/injection")
	for _, def := range schemadrift.Defs(t, "hook.schema.json") {
		if name, ok := strings.CutSuffix(def, "_output"); ok && !slices.Contains(points, name) {
			t.Errorf("schema defines %s for a point the kernel does not have", def)
		}
	}
}
