/*
Copyright 2017 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubernetes/kompose/pkg/kobject"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// writeComposeProject materializes files in a fresh temp dir and returns the
// path of the compose file. Every test controls its own environment; nothing
// here reads the real machine environment.
func writeComposeProject(t *testing.T, composeName, compose string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	p := filepath.Join(dir, composeName)
	if err := os.WriteFile(p, []byte(compose), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	return p
}

func envMap(envs []kobject.EnvVar) map[string]string {
	m := make(map[string]string, len(envs))
	for _, e := range envs {
		m[e.Name] = e.Value
	}
	return m
}

func captureWarnings(t *testing.T) *test.Hook {
	t.Helper()
	oldLevel := log.GetLevel()
	log.SetLevel(log.WarnLevel)
	hook := test.NewGlobal()
	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		hook.Reset()
	})
	return hook
}

func warnMessages(hook *test.Hook) string {
	var b strings.Builder
	for _, entry := range hook.AllEntries() {
		b.WriteString(entry.Message)
		b.WriteString("\n")
	}
	return b.String()
}

func TestLoadFile_InterpolationForms(t *testing.T) {
	compose := `services:
  app:
    image: nginx
    environment:
      SET: ${SET_ME}
      EMPTY: ${EMPTY_ME}
      DEF_COLON: ${MISSING:-dflt}
      DEF_NOSET: ${MISSING-dflt}
      DEF_COLON_ON_EMPTY: ${EMPTY_ME:-dflt}
      DEF_NOSET_ON_EMPTY: ${EMPTY_ME-dflt}
      DOLLAR: $$literal
      QUOTED: "${SET_ME}"
`
	p := writeComposeProject(t, "docker-compose.yaml", compose, nil)

	t.Run("interpolated", func(t *testing.T) {
		// Empty values must be explicitly controlled too: t.Setenv protects us
		// from inheriting the machine environment.
		t.Setenv("SET_ME", "from-host")
		t.Setenv("EMPTY_ME", "")
		// MISSING has no way to "unset" via t.Setenv; the checker treats an
		// unset variable and an absent one the same, so set it to the empty
		// string only where the distinction matters (below) and unset here.
		os.Unsetenv("MISSING")

		ko, err := (&Compose{}).LoadFile([]string{p}, nil, false)
		if err != nil {
			t.Fatalf("LoadFile: %v", err)
		}
		got := envMap(ko.ServiceConfigs["app"].Environment)
		want := map[string]string{
			"SET":                "from-host",
			"EMPTY":              "",
			"DEF_COLON":          "dflt",
			"DEF_NOSET":          "dflt",
			"DEF_COLON_ON_EMPTY": "dflt",
			// `${EMPTY_ME-dflt}` uses the value even though it is empty: only
			// `:-` treats empty as unset.
			"DEF_NOSET_ON_EMPTY": "",
			"DOLLAR":             "$literal",
			"QUOTED":             "from-host",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: want %q, got %q", k, v, got[k])
			}
		}
	})

	t.Run("required variable missing is an error", func(t *testing.T) {
		p := writeComposeProject(t, "docker-compose.yaml",
			"services:\n  app:\n    image: nginx\n    environment:\n      X: ${MUST_BE_SET:?please provide it}\n", nil)
		os.Unsetenv("MUST_BE_SET")
		_, err := (&Compose{}).LoadFile([]string{p}, nil, false)
		if err == nil {
			t.Fatal("expected an error for a missing required variable")
		}
		if !strings.Contains(err.Error(), "MUST_BE_SET") {
			t.Errorf("error should name the variable: %v", err)
		}
	})

	t.Run("no-interpolate keeps placeholders verbatim", func(t *testing.T) {
		t.Setenv("SET_ME", "from-host")
		ko, err := (&Compose{}).LoadFile([]string{p}, nil, true)
		if err != nil {
			t.Fatalf("LoadFile: %v", err)
		}
		got := envMap(ko.ServiceConfigs["app"].Environment)
		want := map[string]string{
			"SET":                "${SET_ME}",
			"EMPTY":              "${EMPTY_ME}",
			"DEF_COLON":          "${MISSING:-dflt}",
			"DEF_NOSET":          "${MISSING-dflt}",
			"DEF_COLON_ON_EMPTY": "${EMPTY_ME:-dflt}",
			"DEF_NOSET_ON_EMPTY": "${EMPTY_ME-dflt}",
			// `$$` is an escape for a literal `$` and is kept as-is when
			// interpolation is disabled.
			"DOLLAR": "$$literal",
			"QUOTED": "${SET_ME}",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: want %q, got %q", k, v, got[k])
			}
		}
	})
}

func TestLoadFile_HostOnlyVariable(t *testing.T) {
	compose := `services:
  app:
    image: nginx
    environment:
      - FROM_HOST
      - MISSING_HOST
`
	p := writeComposeProject(t, "docker-compose.yaml", compose, nil)
	t.Setenv("FROM_HOST", "host-value")
	os.Unsetenv("MISSING_HOST")

	hook := captureWarnings(t)
	ko, err := (&Compose{}).LoadFile([]string{p}, nil, false)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	got := envMap(ko.ServiceConfigs["app"].Environment)
	if got["FROM_HOST"] != "host-value" {
		t.Errorf("FROM_HOST: want host-value, got %q", got["FROM_HOST"])
	}
	if _, ok := got["MISSING_HOST"]; ok {
		t.Errorf("MISSING_HOST should be dropped when the host does not define it")
	}

	msg := warnMessages(hook)
	if !strings.Contains(msg, "MISSING_HOST") || !strings.Contains(msg, "app") {
		t.Errorf("expected a warning naming service and variable, got %q", msg)
	}
	if strings.Contains(msg, "host-value") {
		t.Errorf("warning must not leak a value: %q", msg)
	}
}

func TestLoadFile_EnvFilePriority(t *testing.T) {
	files := map[string]string{
		"a.env": "SHARED=from-a\nONLY_A=a\n",
		"b.env": "SHARED=from-b\nONLY_B=b\n",
	}
	compose := `services:
  app:
    image: nginx
    env_file:
      - a.env
      - b.env
    environment:
      SHARED: from-environment
`
	p := writeComposeProject(t, "docker-compose.yaml", compose, files)

	ko, err := (&Compose{}).LoadFile([]string{p}, nil, false)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	svc := ko.ServiceConfigs["app"]

	// environment must be recorded with its own value (it wins at the
	// container level, see ConfigEnvs); the env_file list must be preserved in
	// order so later files override earlier ones inside their ConfigMaps.
	if got := envMap(svc.Environment)["SHARED"]; got != "from-environment" {
		t.Errorf("SHARED from environment: want %q, got %q", "from-environment", got)
	}
	if len(svc.EnvFiles) != 2 || filepath.Base(svc.EnvFiles[0].Path) != "a.env" || filepath.Base(svc.EnvFiles[1].Path) != "b.env" {
		t.Fatalf("env_file order not preserved: %#v", svc.EnvFiles)
	}
}

func TestLoadFile_EnvFileRequired(t *testing.T) {
	composeOptionalMissing := `services:
  app:
    image: nginx
    env_file:
      - path: missing.env
        required: false
      - present.env
`
	files := map[string]string{"present.env": "X=1\n"}

	t.Run("optional missing file is skipped", func(t *testing.T) {
		p := writeComposeProject(t, "docker-compose.yaml", composeOptionalMissing, files)
		hook := captureWarnings(t)
		ko, err := (&Compose{}).LoadFile([]string{p}, nil, false)
		if err != nil {
			t.Fatalf("LoadFile: %v", err)
		}
		svc := ko.ServiceConfigs["app"]
		if len(svc.EnvFiles) != 1 || filepath.Base(svc.EnvFiles[0].Path) != "present.env" {
			t.Fatalf("only present.env should remain, got %#v", svc.EnvFiles)
		}
		if msg := warnMessages(hook); !strings.Contains(msg, "missing.env") {
			t.Errorf("expected a warning mentioning the skipped file, got %q", msg)
		}
	})

	t.Run("required missing file is an error naming path and service", func(t *testing.T) {
		p := writeComposeProject(t, "docker-compose.yaml",
			"services:\n  app:\n    image: nginx\n    env_file:\n      - missing.env\n", files)
		_, err := (&Compose{}).LoadFile([]string{p}, nil, false)
		if err == nil {
			t.Fatal("expected an error for a missing required env_file")
		}
		if !strings.Contains(err.Error(), "missing.env") || !strings.Contains(err.Error(), "app") {
			t.Errorf("error should mention the file and the service: %v", err)
		}
	})
}

func TestLoadFile_NoInterpolateKeepsPlaceholderInEnvFile(t *testing.T) {
	compose := `services:
  app:
    image: nginx
    env_file:
      - app.env
`
	files := map[string]string{"app.env": "A=${SET_ME}\n"}

	p := writeComposeProject(t, "docker-compose.yaml", compose, files)
	t.Setenv("SET_ME", "ignored")

	// compose-go loads env_file content itself during model resolution; the
	// loader must not expand anything in no-interpolate mode beyond what
	// compose-go does.
	ko, err := (&Compose{}).LoadFile([]string{p}, nil, true)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// The env_file stays as an EnvFile reference; the loader does not fold its
	// contents into Environment.
	if len(ko.ServiceConfigs["app"].EnvFiles) != 1 {
		t.Fatalf("expected the env_file reference to be kept, got %#v", ko.ServiceConfigs["app"].EnvFiles)
	}
}

func TestLoadFile_SecretSources(t *testing.T) {
	compose := `services:
  app:
    image: nginx
secrets:
  from_env:
    environment: SECRET_ENV_VALUE
  from_file:
    file: ./secret.txt
`
	files := map[string]string{"secret.txt": "file-content"}
	p := writeComposeProject(t, "docker-compose.yaml", compose, files)
	t.Setenv("SECRET_ENV_VALUE", "env-content")

	ko, err := (&Compose{}).LoadFile([]string{p}, nil, false)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	env := ko.Secrets["from_env"]
	if env.Environment != "SECRET_ENV_VALUE" {
		t.Errorf("from_env.Environment: got %q", env.Environment)
	}
	if filepath.Base(ko.Secrets["from_file"].File) != "secret.txt" {
		t.Errorf("from_file.File: got %q", ko.Secrets["from_file"].File)
	}
}
