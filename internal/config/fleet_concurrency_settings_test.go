package config

import "testing"

func TestResolveFleetConcurrencySettingsDefaults(t *testing.T) {
	got, err := ResolveFleetConcurrencySettings(nil)
	if err != nil {
		t.Fatalf("ResolveFleetConcurrencySettings: %v", err)
	}
	if got.MinLiveWorkers != 5 || got.MaxLiveWorkers != 10 {
		t.Fatalf("settings = %+v, want min=5 max=10", got)
	}
}

func TestResolveFleetConcurrencySettingsStoredValues(t *testing.T) {
	got, err := ResolveFleetConcurrencySettings(map[string]string{
		FleetMinLiveWorkersKey: "3",
		FleetMaxLiveWorkersKey: "8",
	})
	if err != nil {
		t.Fatalf("ResolveFleetConcurrencySettings: %v", err)
	}
	if got.MinLiveWorkers != 3 || got.MaxLiveWorkers != 8 {
		t.Fatalf("settings = %+v, want min=3 max=8", got)
	}
}

func TestResolveFleetConcurrencySettingsRejectsInvalidBand(t *testing.T) {
	_, err := ResolveFleetConcurrencySettings(map[string]string{
		FleetMinLiveWorkersKey: "11",
		FleetMaxLiveWorkersKey: "10",
	})
	if err == nil {
		t.Fatal("expected min > max to be rejected")
	}
}

func TestFleetConcurrencySettingsAreFleetOnly(t *testing.T) {
	for _, key := range []string{FleetMinLiveWorkersKey, FleetMaxLiveWorkersKey} {
		spec, ok := FleetSettingSpecByKey(key)
		if !ok {
			t.Fatalf("missing setting %q", key)
		}
		if !spec.FleetOnly || spec.Kind != SettingKindInt || spec.Default == "" {
			t.Fatalf("spec = %+v, want fleet-only int with default", spec)
		}
	}
}

func TestNormalizeFleetConcurrencySettingRejectsNegative(t *testing.T) {
	if _, err := NormalizeSettingValue(FleetMaxLiveWorkersKey, "-1"); err == nil {
		t.Fatal("expected negative fleet max to be rejected")
	}
}
