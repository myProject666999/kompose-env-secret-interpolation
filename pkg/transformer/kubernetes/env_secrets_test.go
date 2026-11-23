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

package kubernetes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/kubernetes/kompose/pkg/kobject"
	"github.com/kubernetes/kompose/pkg/loader/compose"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	appsv1 "k8s.io/api/apps/v1"
	api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func captureLogs(t *testing.T) *test.Hook {
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

func logText(hook *test.Hook) string {
	var b strings.Builder
	for _, entry := range hook.AllEntries() {
		b.WriteString(entry.Message)
		b.WriteString("\n")
	}
	return b.String()
}

const secretValueSentinel = "S3CR3T-VALUE-DO-NOT-LEAK"

// writeEnvFixture creates a temp compose file plus sibling files and returns
// the compose file path. Only the returned directory is touched.
func writeEnvFixture(t *testing.T, compose string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	p := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(p, []byte(compose), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	return p
}

func TestConfigEnvs_EnvironmentOverridesEnvFile(t *testing.T) {
	composeFile := writeEnvFixture(t, `services:
  app:
    image: nginx
    env_file:
      - app.env
    environment:
      SHARED: from-environment
`, map[string]string{"app.env": "SHARED=from-file\nONLY_FILE=file\n"})

	service := kobject.ServiceConfig{
		Name: "app",
		EnvFiles: []kobject.EnvFile{
			{Path: "app.env", Required: true},
		},
		Environment: []kobject.EnvVar{{Name: "SHARED", Value: "from-environment"}},
	}
	envs, envFrom, err := ConfigEnvs(service, kobject.ConvertOptions{InputFiles: []string{composeFile}})
	if err != nil {
		t.Fatalf("ConfigEnvs: %v", err)
	}
	if len(envFrom) != 1 || envFrom[0].ConfigMapRef == nil || envFrom[0].ConfigMapRef.Name != "app-env" {
		t.Fatalf("expected one envFrom ConfigMap reference, got %#v", envFrom)
	}
	found := false
	for _, e := range envs {
		if e.Name == "SHARED" {
			found = true
			if e.Value != "from-environment" {
				t.Errorf("SHARED: want the environment value to win, got %q", e.Value)
			}
		}
	}
	if !found {
		t.Errorf("SHARED must be present in the explicit env list so that it overrides envFrom")
	}
}

func TestPargeEnvFiletoConfigMaps_LastFileWins(t *testing.T) {
	composeFile := writeEnvFixture(t, `services:
  app:
    image: nginx
    env_file:
      - a.env
      - b.env
`, map[string]string{
		"a.env": "SHARED=from-a\nONLY_A=a\n",
		"b.env": "SHARED=from-b\nONLY_B=b\n",
	})

	service := kobject.ServiceConfig{
		Name: "app",
		EnvFiles: []kobject.EnvFile{
			{Path: "a.env", Required: true},
			{Path: "b.env", Required: true},
		},
	}
	k := Kubernetes{}
	objs, err := k.PargeEnvFiletoConfigMaps("app", service, kobject.ConvertOptions{InputFiles: []string{composeFile}})
	if err != nil {
		t.Fatalf("PargeEnvFiletoConfigMaps: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected two ConfigMaps, got %d", len(objs))
	}
	first := objs[0].(*api.ConfigMap)
	second := objs[1].(*api.ConfigMap)
	if first.Data["SHARED"] != "from-a" {
		t.Errorf("first file SHARED: want from-a, got %q", first.Data["SHARED"])
	}
	if second.Data["SHARED"] != "from-b" {
		t.Errorf("later file must win: want from-b, got %q", second.Data["SHARED"])
	}
}

func TestPargeEnvFiletoConfigMaps_EnvFileUsesServiceEnvironment(t *testing.T) {
	composeFile := writeEnvFixture(t, `services:
  app:
    image: nginx
    env_file:
      - app.env
    environment:
      DOC_ENGINE: test-env
`, map[string]string{"app.env": "A=${DOC_ENGINE}\nB=${SOMETHING_ELSE:-fallback}\n"})

	service := kobject.ServiceConfig{
		Name:        "app",
		EnvFiles:    []kobject.EnvFile{{Path: "app.env", Required: true}},
		Environment: []kobject.EnvVar{{Name: "DOC_ENGINE", Value: "test-env"}},
	}
	k := Kubernetes{}
	objs, err := k.PargeEnvFiletoConfigMaps("app", service, kobject.ConvertOptions{InputFiles: []string{composeFile}})
	if err != nil {
		t.Fatalf("PargeEnvFiletoConfigMaps: %v", err)
	}
	cm := objs[0].(*api.ConfigMap)
	if cm.Data["A"] != "test-env" {
		t.Errorf("A: env_file should see the service environment, got %q", cm.Data["A"])
	}
	if cm.Data["B"] != "fallback" {
		t.Errorf("B: default should apply, got %q", cm.Data["B"])
	}
}

func TestPargeEnvFiletoConfigMaps_SensitiveKeyWarningDoesNotLeakValue(t *testing.T) {
	composeFile := writeEnvFixture(t, `services:
  app:
    image: nginx
    env_file:
      - app.env
`, map[string]string{"app.env": "DB_PASSWORD=" + secretValueSentinel + "\nAPI_TOKEN=" + secretValueSentinel + "\n"})

	service := kobject.ServiceConfig{
		Name:     "app",
		EnvFiles: []kobject.EnvFile{{Path: "app.env", Required: true}},
	}
	hook := captureLogs(t)
	k := Kubernetes{}
	if _, err := k.PargeEnvFiletoConfigMaps("app", service, kobject.ConvertOptions{InputFiles: []string{composeFile}}); err != nil {
		t.Fatalf("PargeEnvFiletoConfigMaps: %v", err)
	}
	msg := logText(hook)
	if !strings.Contains(msg, "DB_PASSWORD") || !strings.Contains(msg, "API_TOKEN") {
		t.Errorf("expected a warning naming the sensitive keys, got %q", msg)
	}
	if strings.Contains(msg, secretValueSentinel) {
		t.Errorf("warning leaked a secret value: %q", msg)
	}
}

func TestPargeEnvFiletoConfigMaps_StableOutput(t *testing.T) {
	composeFile := writeEnvFixture(t, `services:
  app:
    image: nginx
    env_file:
      - app.env
`, map[string]string{"app.env": "Z=1\nA=2\nM=3\n"})

	service := kobject.ServiceConfig{
		Name:     "app",
		EnvFiles: []kobject.EnvFile{{Path: "app.env", Required: true}},
	}
	var first string
	for i := 0; i < 5; i++ {
		k := Kubernetes{}
		objs, err := k.PargeEnvFiletoConfigMaps("app", service, kobject.ConvertOptions{InputFiles: []string{composeFile}})
		if err != nil {
			t.Fatalf("PargeEnvFiletoConfigMaps: %v", err)
		}
		cm := objs[0].(*api.ConfigMap)
		// encoding/json sorts map keys, which is what the printer ultimately does.
		data, err := json.Marshal(cm)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if i == 0 {
			first = string(data)
			continue
		}
		if string(data) != first {
			t.Fatalf("ConfigMap serialization is not stable across runs")
		}
	}
}

func TestCreateSecrets_FromEnvironment(t *testing.T) {
	t.Setenv("SECRET_ENV_VALUE", secretValueSentinel)
	ko := kobject.KomposeObject{
		Secrets: types.Secrets{
			"mysecret": types.SecretConfig{Environment: "SECRET_ENV_VALUE"},
		},
	}
	k := Kubernetes{}
	secrets, err := k.CreateSecrets(ko)
	if err != nil {
		t.Fatalf("CreateSecrets: %v", err)
	}
	if len(secrets) != 1 {
		t.Fatalf("expected one secret, got %d", len(secrets))
	}
	data := secrets[0].Data["mysecret"]
	if string(data) != secretValueSentinel {
		t.Errorf("secret data: want the environment value, got %q", string(data))
	}
}

func TestCreateSecrets_EnvironmentEmptyIsRejected(t *testing.T) {
	t.Setenv("SECRET_ENV_VALUE", "")
	ko := kobject.KomposeObject{
		Secrets: types.Secrets{
			"mysecret": types.SecretConfig{Environment: "SECRET_ENV_VALUE"},
		},
	}
	k := Kubernetes{}
	_, err := k.CreateSecrets(ko)
	if err == nil {
		t.Fatal("expected an error for an empty environment-sourced secret")
	}
	if !strings.Contains(err.Error(), "mysecret") || !strings.Contains(err.Error(), "SECRET_ENV_VALUE") {
		t.Errorf("error should name the secret and the variable: %v", err)
	}
}

func TestCreateSecrets_ExternalIgnoredWithWarning(t *testing.T) {
	ko := kobject.KomposeObject{
		Secrets: types.Secrets{
			"ext": types.SecretConfig{External: true},
		},
	}
	hook := captureLogs(t)
	k := Kubernetes{}
	secrets, err := k.CreateSecrets(ko)
	if err != nil {
		t.Fatalf("CreateSecrets: %v", err)
	}
	if len(secrets) != 0 {
		t.Errorf("external secrets should be ignored, got %d", len(secrets))
	}
	if msg := logText(hook); !strings.Contains(msg, "ext") {
		t.Errorf("expected a warning for the ignored external secret, got %q", msg)
	}
}

func TestCreateSecrets_StableOrder(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "s.txt")
	if err := os.WriteFile(file, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	ko := kobject.KomposeObject{
		Secrets: types.Secrets{
			"bravo":   types.SecretConfig{File: file},
			"alpha":   types.SecretConfig{File: file},
			"charlie": types.SecretConfig{File: file},
		},
	}
	var names []string
	for i := 0; i < 5; i++ {
		k := Kubernetes{}
		secrets, err := k.CreateSecrets(ko)
		if err != nil {
			t.Fatalf("CreateSecrets: %v", err)
		}
		var got []string
		for _, s := range secrets {
			got = append(got, s.Name)
		}
		joined := strings.Join(got, ",")
		if i == 0 {
			names = got
			continue
		}
		if joined != strings.Join(names, ",") {
			t.Fatalf("secret order is not stable: %v vs %v", got, names)
		}
	}
	if strings.Join(names, ",") != "alpha,bravo,charlie" {
		t.Errorf("secrets should be sorted by name, got %v", names)
	}
}

func TestConfigEnvs_SecretReferenceRewrite(t *testing.T) {
	service := kobject.ServiceConfig{
		Name: "app",
		Secrets: []types.ServiceSecretConfig{
			{Source: "my_secret"},
		},
		Environment: []kobject.EnvVar{
			{Name: "HIT", Value: "/run/secrets/my_secret"},
			{Name: "MISS_UNDECLARED", Value: "/run/secrets/other_secret"},
			{Name: "URL_MISS", Value: "https://example.com/run/secrets/my_secret"},
			{Name: "PREFIX_MISS", Value: "prefix/run/secrets/my_secret"},
		},
	}
	envs, _, err := ConfigEnvs(service, kobject.ConvertOptions{})
	if err != nil {
		t.Fatalf("ConfigEnvs: %v", err)
	}
	got := make(map[string]string, len(envs))
	for _, e := range envs {
		got[e.Name] = e.Value
	}
	cases := map[string]string{
		"HIT":             "my-secret",
		"MISS_UNDECLARED": "/run/secrets/other_secret",
		"URL_MISS":        "https://example.com/run/secrets/my_secret",
		"PREFIX_MISS":     "prefix/run/secrets/my_secret",
	}
	for name, want := range cases {
		if got[name] != want {
			t.Errorf("%s: want %q, got %q", name, want, got[name])
		}
	}
}

func TestGetSecretPaths_Matrix(t *testing.T) {
	cases := []struct {
		target        string
		asFiles       bool
		wantItemPath  string
		wantMountPath string
		wantSubPath   string
	}{
		{"", true, "the-secret", "/run/secrets/the-secret", "the-secret"},
		{"foo", true, "foo", "/run/secrets/foo", "foo"},
		{"/abs/foo", true, "foo", "/abs/foo", "foo"},
		{"dir/foo", true, "foo", "/run/secrets/dir/foo", "foo"},
		{"/abs/dir/foo", true, "foo", "/abs/dir/foo", "foo"},
		{"", false, "the-secret", "/run/secrets/the-secret", "the-secret"},
		{"foo", false, "foo", "/run/secrets/the-secret", "foo"},
		{"/abs/foo", false, "foo", "/abs", "foo"},
		{"dir/foo", false, "foo", "/run/secrets/dir", "foo"},
		{"/abs/dir/foo", false, "foo", "/abs/dir", "foo"},
	}
	for _, tc := range cases {
		k := Kubernetes{Opt: kobject.ConvertOptions{SecretsAsFiles: tc.asFiles}}
		sc := types.ServiceSecretConfig{Source: "the-secret", Target: tc.target}
		var item, mount, sub string
		if tc.asFiles {
			item, mount, sub = k.getSecretPaths(sc)
		} else {
			item, mount, sub = k.getSecretPathsLegacy(sc)
		}
		if item != tc.wantItemPath || mount != tc.wantMountPath || sub != tc.wantSubPath {
			t.Errorf("asFiles=%v target=%q: got (%q,%q,%q), want (%q,%q,%q)",
				tc.asFiles, tc.target, item, mount, sub, tc.wantItemPath, tc.wantMountPath, tc.wantSubPath)
		}
	}
}

func TestParseEnvFile_UnifiedSyntax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.env")
	body := "\uFEFFexport EXPORTED=yes\n" +
		"QUOTED=\"line1\nline2\"\n" +
		"INLINE=value # comment\n" +
		"DUP=one\nDUP=two\n" +
		"REF=${LOOKUP_ME}\n" +
		"REFDEF=${MISSING_X:-fallback}\n" +
		"DOLLAR=$$literal\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOOKUP_ME", "looked-up")
	os.Unsetenv("MISSING_X")

	envs := map[string]string{}
	if err := ParseEnvFile(path, "", envs, func(key string) (string, bool) {
		v, ok := os.LookupEnv(key)
		return v, ok
	}); err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	want := map[string]string{
		"EXPORTED": "yes",
		"QUOTED":   "line1\nline2",
		"INLINE":   "value",
		"DUP":      "two",
		"REF":      "looked-up",
		"REFDEF":   "fallback",
		"DOLLAR":   "$literal",
	}
	if len(envs) != len(want) {
		t.Errorf("key count: want %d, got %d (%#v)", len(want), len(envs), envs)
	}
	for k, v := range want {
		if envs[k] != v {
			t.Errorf("%s: want %q, got %q", k, v, envs[k])
		}
	}
}

func TestParseEnvFile_SameKeySetForValidationAndConfigMap(t *testing.T) {
	// A file whose keys are only visible to one parser would previously be
	// dropped from the environment yet missing from the ConfigMap. Both entry
	// points must agree.
	dir := t.TempDir()
	path := filepath.Join(dir, "app.env")
	body := "\uFEFFBOM=1\nexport EXPORTED=2\nDUP=a\nDUP=b\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	validation, err := GetEnvsFromFile(path)
	if err != nil {
		t.Fatalf("GetEnvsFromFile: %v", err)
	}
	generated := map[string]string{}
	if err := ParseEnvFile(path, "", generated, nil); err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	if len(validation) != len(generated) {
		t.Fatalf("key sets differ: validation=%v generated=%v", validation, generated)
	}
	for k := range validation {
		if validation[k] != generated[k] {
			t.Errorf("%s: validation=%q generated=%q", k, validation[k], generated[k])
		}
	}
}

func TestCreateSecrets_ContentWithoutSourceIsRejected(t *testing.T) {
	ko := kobject.KomposeObject{
		Secrets: types.Secrets{
			"inline": types.SecretConfig{Content: secretValueSentinel},
		},
	}
	k := Kubernetes{}
	hook := captureLogs(t)
	_, err := k.CreateSecrets(ko)
	if err == nil {
		t.Fatal("expected an error for a content-only secret")
	}
	if strings.Contains(err.Error(), secretValueSentinel) || strings.Contains(logText(hook), secretValueSentinel) {
		t.Errorf("error/log leaked the secret value: err=%v log=%q", err, logText(hook))
	}
}

// TestTransform_EndToEndEnvMatrix drives the loader and the transformer together
// and inspects the generated objects. It never reads the machine environment:
// every variable it depends on is set with t.Setenv, and all files live in a
// temp directory.
func TestTransform_EndToEndEnvMatrix(t *testing.T) {
	t.Setenv("FROM_HOST", "host-value")
	os.Unsetenv("MISSING_FROM_HOST")

	compose := `services:
  app:
    image: nginx
    env_file:
      - a.env
      - b.env
    environment:
      - FROM_HOST
      - MISSING_FROM_HOST
      - SHARED=from-environment
`
	files := map[string]string{
		"a.env": "SHARED=from-a\nONLY_A=a\nDB_PASSWORD=" + secretValueSentinel + "\n",
		"b.env": "SHARED=from-b\nONLY_B=b\n",
	}
	composeDir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(composeDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	composeFile := filepath.Join(composeDir, "docker-compose.yaml")
	if err := os.WriteFile(composeFile, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}

	ko, err := composeLoader().LoadFile([]string{composeFile}, nil, false)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	hook := captureLogs(t)
	k := Kubernetes{}
	objects, err := k.Transform(ko, kobject.ConvertOptions{
		CreateD:               true,
		InputFiles:            []string{composeFile},
		WithKomposeAnnotation: false,
	})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	env := containerEnv(t, objects, "app")
	// environment wins over env_file
	if env["SHARED"] != "from-environment" {
		t.Errorf("SHARED: want environment to win, got %q", env["SHARED"])
	}
	// host-only variable is populated
	if env["FROM_HOST"] != "host-value" {
		t.Errorf("FROM_HOST: want host-value, got %q", env["FROM_HOST"])
	}
	// missing host variable is dropped
	if _, ok := env["MISSING_FROM_HOST"]; ok {
		t.Errorf("MISSING_FROM_HOST should be dropped")
	}

	// Each env_file becomes a ConfigMap; the later file wins inside its own map
	// while the earlier map keeps its value.
	bySuffix := configMapsBySuffix(t, objects)
	if bySuffix["a-env"] == nil || bySuffix["a-env"].Data["SHARED"] != "from-a" {
		t.Errorf("a-env SHARED: want from-a, got %#v", bySuffix["a-env"])
	}
	if bySuffix["b-env"] == nil || bySuffix["b-env"].Data["SHARED"] != "from-b" {
		t.Errorf("b-env SHARED: want from-b, got %#v", bySuffix["b-env"])
	}

	// Sensitive keys are warned about without leaking the value.
	msg := logText(hook)
	if !strings.Contains(msg, "DB_PASSWORD") {
		t.Errorf("expected a warning about DB_PASSWORD, got %q", msg)
	}
	if strings.Contains(msg, secretValueSentinel) {
		t.Errorf("logs leaked the secret value: %q", msg)
	}
}

// composeLoader returns a loader for compose files. Kept as a helper so the
// end-to-end test reads cleanly.
func composeLoader() *compose.Compose {
	return &compose.Compose{}
}

// containerEnv returns the explicit env map of the named container across all
// workload objects.
func containerEnv(t *testing.T, objects []runtime.Object, service string) map[string]string {
	t.Helper()
	for _, obj := range objects {
		var containers []api.Container
		switch o := obj.(type) {
		case *appsv1.Deployment:
			containers = o.Spec.Template.Spec.Containers
		case *api.Pod:
			containers = o.Spec.Containers
		default:
			continue
		}
		for _, c := range containers {
			env := make(map[string]string, len(c.Env))
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
			return env
		}
	}
	t.Fatalf("no workload with containers found")
	return nil
}

func configMapsBySuffix(t *testing.T, objects []runtime.Object) map[string]*api.ConfigMap {
	t.Helper()
	m := map[string]*api.ConfigMap{}
	for _, obj := range objects {
		if cm, ok := obj.(*api.ConfigMap); ok {
			for _, suffix := range []string{"a-env", "b-env"} {
				if strings.HasSuffix(cm.Name, suffix) {
					m[suffix] = cm
				}
			}
		}
	}
	return m
}

func TestPargeEnvFiletoConfigMaps_NoInterpolateWarnsAboutPlaceholder(t *testing.T) {
	// env_file content is interpolated by the dotenv parser regardless of
	// --no-interpolate, so the only way a literal ${...} survives is through
	// the $$ escape. That surviving placeholder must be reported.
	composeFile := writeEnvFixture(t, `services:
  app:
    image: nginx
    env_file:
      - app.env
`, map[string]string{"app.env": "PLACEHOLDER=$${SOME_VAR}\n"})

	service := kobject.ServiceConfig{
		Name:     "app",
		EnvFiles: []kobject.EnvFile{{Path: "app.env", Required: true}},
	}
	hook := captureLogs(t)
	k := Kubernetes{}
	objs, err := k.PargeEnvFiletoConfigMaps("app", service, kobject.ConvertOptions{
		InputFiles:    []string{composeFile},
		NoInterpolate: true,
	})
	if err != nil {
		t.Fatalf("PargeEnvFiletoConfigMaps: %v", err)
	}
	cm := objs[0].(*api.ConfigMap)
	// The placeholder is preserved verbatim in no-interpolate mode.
	if cm.Data["PLACEHOLDER"] != "${SOME_VAR}" {
		t.Errorf("PLACEHOLDER: want the literal placeholder, got %q", cm.Data["PLACEHOLDER"])
	}
	if msg := logText(hook); !strings.Contains(msg, "PLACEHOLDER") {
		t.Errorf("expected a warning naming the key with an unresolved placeholder, got %q", msg)
	}
}

func TestInitConfigMapFromContent_KeyFromTarget(t *testing.T) {
	service := kobject.ServiceConfig{Name: "app"}
	k := Kubernetes{}
	cm := k.InitConfigMapFromContent("app", service, "hello", "inline_cfg", "/etc/app/inline.conf")
	if cm.Name != "inline_cfg" {
		t.Errorf("ConfigMap name: want inline_cfg, got %q", cm.Name)
	}
	if cm.Data["inline.conf"] != "hello" {
		t.Errorf("data key should be the target basename, got %#v", cm.Data)
	}
}
