package v1

import (
	"encoding/json"
	"testing"
)

func TestTerraformSpecMarshalJSONIncludesFalseBooleans(t *testing.T) {
	t.Parallel()

	spec := TerraformSpec{
		TerraformModule:  Module{Inline: `output "hello" { value = "world" }`},
		TerraformVersion: "1.6.0",
		Backend:          `terraform {}`,
		TaskOptions:      []TaskOption{},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal terraform spec: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal terraform spec: %v", err)
	}

	for _, key := range []string{
		"keepLatestPodsOnly",
		"keepCompletedPods",
		"writeOutputsToStatus",
		"ignoreDelete",
		"requireApproval",
	} {
		value, ok := got[key]
		if !ok {
			t.Fatalf("expected %q to be present", key)
		}
		if value != false {
			t.Fatalf("expected %q to marshal as false, got %#v", key, value)
		}
	}

	taskOptions, ok := got["taskOptions"].([]any)
	if !ok {
		t.Fatalf("expected taskOptions to marshal as an array, got %#v", got["taskOptions"])
	}
	if len(taskOptions) != 0 {
		t.Fatalf("expected empty taskOptions array, got %#v", taskOptions)
	}
}

func TestTofuSpecMarshalJSONIncludesFalseBooleans(t *testing.T) {
	t.Parallel()

	spec := TofuSpec{
		TofuModule:  Module{Inline: `output "hello" { value = "world" }`},
		TofuVersion: "1.9.0",
		Backend:     `terraform {}`,
		TaskOptions: []TaskOption{},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal tofu spec: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal tofu spec: %v", err)
	}

	for _, key := range []string{
		"keepLatestPodsOnly",
		"keepCompletedPods",
		"writeOutputsToStatus",
		"ignoreDelete",
		"requireApproval",
	} {
		value, ok := got[key]
		if !ok {
			t.Fatalf("expected %q to be present", key)
		}
		if value != false {
			t.Fatalf("expected %q to marshal as false, got %#v", key, value)
		}
	}

	taskOptions, ok := got["taskOptions"].([]any)
	if !ok {
		t.Fatalf("expected taskOptions to marshal as an array, got %#v", got["taskOptions"])
	}
	if len(taskOptions) != 0 {
		t.Fatalf("expected empty taskOptions array, got %#v", taskOptions)
	}
}

func TestSetupAndPluginMarshalJSONKeepExplicitFalseValues(t *testing.T) {
	t.Parallel()

	setupData, err := json.Marshal(Setup{})
	if err != nil {
		t.Fatalf("marshal setup: %v", err)
	}
	var setupJSON map[string]any
	if err := json.Unmarshal(setupData, &setupJSON); err != nil {
		t.Fatalf("unmarshal setup: %v", err)
	}
	if value, ok := setupJSON["cleanupDisk"]; !ok || value != false {
		t.Fatalf("expected cleanupDisk=false, got %#v", setupJSON["cleanupDisk"])
	}

	pluginData, err := json.Marshal(Plugin{When: "After", Task: RunPlan})
	if err != nil {
		t.Fatalf("marshal plugin: %v", err)
	}
	var pluginJSON map[string]any
	if err := json.Unmarshal(pluginData, &pluginJSON); err != nil {
		t.Fatalf("unmarshal plugin: %v", err)
	}
	if value, ok := pluginJSON["must"]; !ok || value != false {
		t.Fatalf("expected must=false, got %#v", pluginJSON["must"])
	}
}

func TestResourceDownloadMarshalJSONKeepsUseAsVarFalse(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(ResourceDownload{Address: "https://example.com/module.tfvars"})
	if err != nil {
		t.Fatalf("marshal resource download: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal resource download: %v", err)
	}

	if value, ok := got["useAsVar"]; !ok || value != false {
		t.Fatalf("expected useAsVar=false, got %#v", got["useAsVar"])
	}
}
