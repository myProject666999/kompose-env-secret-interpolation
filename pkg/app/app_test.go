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

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kubernetes/kompose/pkg/kobject"
	api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestConvert_AbsoluteEnvFilePath exercises the whole pipeline. The loader
// records env_file paths relative to the compose directory; app.Convert then
// rewrites them to be relative to the working dir. A regression there would make
// the ConfigMap generation read the wrong path (or fail).
func TestConvert_AbsoluteEnvFilePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.env"), []byte("FROM_FILE=file-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compose := `services:
  app:
    image: nginx
    env_file:
      - app.env
    environment:
      FROM_ENV: env-value
`
	composeFile := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(composeFile, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	objs, err := Convert(kobject.ConvertOptions{
		CreateD:    true,
		InputFiles: []string{composeFile},
		OutFile:    outDir + string(os.PathSeparator),
	})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	// The env_file must have been turned into a ConfigMap with the file contents.
	var found bool
	for _, obj := range objs {
		if cm, ok := obj.(*api.ConfigMap); ok {
			if cm.Data["FROM_FILE"] == "file-value" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("expected a ConfigMap holding FROM_FILE=file-value, objects: %v", kindNames(objs))
	}
}

func kindNames(objs []runtime.Object) []string {
	var names []string
	for _, obj := range objs {
		names = append(names, obj.GetObjectKind().GroupVersionKind().Kind)
	}
	return names
}
