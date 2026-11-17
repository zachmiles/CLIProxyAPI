package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelCatalogConfigRoundTrip(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		t.Run(fmt.Sprint(migrate), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			sources := ModelCatalogs{Catalog: filepath.Join(t.TempDir(), "models.json"), CodexCatalog: "https://example.com/codex.json", DevinCatalog: "http://localhost/devin.json"}
			data, errMarshal := yaml.Marshal(map[string]any{"config-version": 8, "models": sources})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if errValidate := ValidateV8Config(data); errValidate != nil {
				t.Fatal(errValidate)
			}
			if errWrite := os.WriteFile(path, data, 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			cfg, errLoad := LoadConfig(path)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			if !reflect.DeepEqual(cfg.Models, sources) {
				t.Fatalf("loaded sources: %+v", cfg.Models)
			}
			if errSave := SaveConfigPreserveComments(path, cfg, migrate); errSave != nil {
				t.Fatal(errSave)
			}
			saved, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			if errValidate := ValidateV8Config(saved); errValidate != nil {
				t.Fatal(errValidate)
			}
			reloaded, errReload := LoadConfig(path)
			if errReload != nil {
				t.Fatal(errReload)
			}
			if !reflect.DeepEqual(reloaded.Models, sources) {
				t.Fatalf("saved sources: %+v", reloaded.Models)
			}
			cfg.Models = ModelCatalogs{}
			if errSave := SaveConfigPreserveComments(path, cfg, migrate); errSave != nil {
				t.Fatal(errSave)
			}
			cleared, errClear := LoadConfig(path)
			if errClear != nil {
				t.Fatal(errClear)
			}
			if !reflect.DeepEqual(cleared.Models, ModelCatalogs{}) {
				t.Fatalf("cleared sources retained: %+v", cleared.Models)
			}

		})
	}
}

func TestModelCatalogConfigValidation(t *testing.T) {
	badChannel := []byte("models: {discovery: {typo-channel: {mode: \"off\"}}}")
	if errValidate := ValidateV8Config(badChannel); errValidate == nil || !strings.Contains(errValidate.Error(), `"typo-channel"`) {
		t.Fatalf("expected discovery validation to name the unsupported channel, got %v", errValidate)
	}
	for _, field := range []string{"catalog", "codex-catalog", "devin-catalog"} {
		for _, source := range []string{"relative.json", "./models.json", "~/models.json", "ftp://example.com/models", "file:///tmp/models.json", "https:///models"} {
			data := []byte(fmt.Sprintf("models:\n  %s: %q\n", field, source))
			var cfg Config
			if errUnmarshal := yaml.Unmarshal(data, &cfg); errUnmarshal == nil {
				t.Fatalf("accepted %s: %s", field, source)
			}
			if errValidate := ValidateV8Config(data); errValidate == nil {
				t.Fatalf("v8 accepted %s: %s", field, source)
			}
		}
	}
	for _, raw := range []string{"models: {}", "models: {catalog: ''}", "models: {codex-catalog: 'https://example.com/models.json'}"} {
		if errValidate := ValidateV8Config([]byte(raw)); errValidate != nil {
			t.Fatal(errValidate)
		}
	}
}
