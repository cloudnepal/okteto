//go:build integration
// +build integration

// Copyright 2023 The Okteto Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deploy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/okteto/okteto/integration"
	"github.com/okteto/okteto/integration/commands"
	"github.com/okteto/okteto/pkg/cmd/pipeline"
	"github.com/okteto/okteto/pkg/k8s/kubeconfig"
	"github.com/okteto/okteto/pkg/okteto"
	"github.com/okteto/okteto/pkg/registry"
	"github.com/stretchr/testify/require"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	oktetoManifestName    = "okteto.yml"
	oktetoManifestContent = `build:
  app:
    context: app
deploy:
  - kubectl apply -f k8s.yml
`
	oktetoManifestContentWithCache = `build:
  app:
    context: app
    export_cache: okteto.dev/app:dev
    cache_from: okteto.dev/app:dev
    image: okteto.dev/app:dev
deploy:
  - kubectl apply -f k8s.yml
`
	oktetoManifestWithDeployRemote = `build:
  app:
    context: app
    image: okteto.dev/app:dev
deploy:
  image: okteto/installer:1.8.9
  commands:
  - name: deploy nginx
    command: kubectl create deployment my-dep --image=busybox`

	appDockerfileWithCache = `FROM python:alpine
EXPOSE 2931
RUN --mount=type=cache,target=/root/.cache echo hola`
	k8sManifestTemplateWithCache = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: e2etest
spec:
  replicas: 1
  selector:
    matchLabels:
      app: e2etest
  template:
    metadata:
      labels:
        app: e2etest
    spec:
      terminationGracePeriodSeconds: 1
      containers:
      - name: test
        image: %s
        ports:
        - containerPort: 8080
        workingDir: /usr/src/app
        env:
          - name: VAR
            value: value1
        command:
            - sh
            - -c
            - "echo -n $VAR > var.html && python -m http.server 8080"
---
apiVersion: v1
kind: Service
metadata:
  name: e2etest
  annotations:
    dev.okteto.com/auto-ingress: "true"
spec:
  type: ClusterIP
  ports:
  - name: e2etest
    port: 8080
  selector:
    app: e2etest
`
	simpleOktetoManifestContent = `deploy:
  - echo "hi! this is a test"
`
)

// TestDeployOktetoManifest tests the following scenario:
// - Deploying a okteto manifest locally
// - The endpoints generated are accessible
func TestDeployOktetoManifest(t *testing.T) {
	t.Parallel()
	oktetoPath, err := integration.GetOktetoPath()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, createOktetoManifest(dir, oktetoManifestContent))
	require.NoError(t, createAppDockerfile(dir))
	require.NoError(t, createK8sManifest(dir))

	testNamespace := integration.GetTestNamespace(t.Name())
	namespaceOpts := &commands.NamespaceOptions{
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoCreateNamespace(oktetoPath, namespaceOpts))
	require.NoError(t, commands.RunOktetoKubeconfig(oktetoPath, &commands.KubeconfigOpts{
		OktetoHome: dir,
	}))
	c, _, err := okteto.NewK8sClientProvider().Provide(kubeconfig.Get([]string{filepath.Join(dir, ".kube", "config")}))
	require.NoError(t, err)

	deployOptions := &commands.DeployOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	// Test that endpoint works
	autowakeURL := fmt.Sprintf("https://e2etest-%s.%s", testNamespace, appsSubdomain)
	require.NotEmpty(t, integration.GetContentFromURL(autowakeURL, timeout))

	// Test that image has been built

	appImageDev := fmt.Sprintf("%s/%s/%s-app:okteto", okteto.GetContext().Registry, testNamespace, filepath.Base(dir))
	require.NotEmpty(t, getImageWithSHA(appImageDev))

	destroyOptions := &commands.DestroyOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
	}
	require.NoError(t, commands.RunOktetoDestroy(oktetoPath, destroyOptions))

	_, err = integration.GetService(context.Background(), testNamespace, "e2etest", c)
	require.True(t, k8sErrors.IsNotFound(err))
	require.NoError(t, commands.RunOktetoDeleteNamespace(oktetoPath, namespaceOpts))
}

// TestDeployOktetoManifest tests the following scenario:
// - Deploying a okteto manifest locally
// - The endpoints generated are accessible
// - Images are only build if
func TestRedeployOktetoManifestForImages(t *testing.T) {
	t.Parallel()
	oktetoPath, err := integration.GetOktetoPath()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, createOktetoManifest(dir, oktetoManifestContent))
	require.NoError(t, createAppDockerfile(dir))
	require.NoError(t, createK8sManifest(dir))

	testNamespace := integration.GetTestNamespace(t.Name())
	namespaceOpts := &commands.NamespaceOptions{
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoCreateNamespace(oktetoPath, namespaceOpts))
	require.NoError(t, commands.RunOktetoKubeconfig(oktetoPath, &commands.KubeconfigOpts{
		OktetoHome: dir,
	}))
	c, _, err := okteto.NewK8sClientProvider().Provide(kubeconfig.Get([]string{filepath.Join(dir, ".kube", "config")}))
	require.NoError(t, err)

	// Test that image is not built before running okteto deploy
	appImageDev := fmt.Sprintf("%s/%s/%s-app:okteto", okteto.GetContext().Registry, testNamespace, filepath.Base(dir))
	require.False(t, isImageBuilt(appImageDev))

	deployOptions := &commands.DeployOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	// Test that image is built after running okteto deploy
	require.True(t, isImageBuilt(appImageDev))

	// Test that endpoint works
	autowakeURL := fmt.Sprintf("https://e2etest-%s.%s", testNamespace, appsSubdomain)
	require.NotEmpty(t, integration.GetContentFromURL(autowakeURL, timeout))

	deployOptions.LogLevel = "debug"
	// Test redeploy is not building any image
	output, err := commands.RunOktetoDeployAndGetOutput(oktetoPath, deployOptions)
	require.NoError(t, err)

	err = expectImageFoundNoSkippingBuild(output)
	require.Error(t, err, err)

	// Test redeploy with build flag builds the image
	deployOptions.Build = true
	output, err = commands.RunOktetoDeployAndGetOutput(oktetoPath, deployOptions)
	require.NoError(t, err)

	require.NoError(t, expectForceBuild(output))

	destroyOptions := &commands.DestroyOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
	}
	require.NoError(t, commands.RunOktetoDestroy(oktetoPath, destroyOptions))

	_, err = integration.GetService(context.Background(), testNamespace, "e2etest", c)
	require.True(t, k8sErrors.IsNotFound(err))
	require.NoError(t, commands.RunOktetoDeleteNamespace(oktetoPath, namespaceOpts))
}

// TestDeployOktetoManifestWithDestroy tests the following scenario:
// - Deploying a okteto manifest locally
// - The endpoints generated are accessible
// - Redeploy with okteto deploy
// - Checks that configmap is still there
func TestDeployOktetoManifestWithDestroy(t *testing.T) {
	t.Parallel()
	oktetoPath, err := integration.GetOktetoPath()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, createOktetoManifest(dir, oktetoManifestContent))
	require.NoError(t, createAppDockerfile(dir))
	require.NoError(t, createK8sManifest(dir))

	testNamespace := integration.GetTestNamespace(t.Name())
	namespaceOpts := &commands.NamespaceOptions{
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoCreateNamespace(oktetoPath, namespaceOpts))
	require.NoError(t, commands.RunOktetoKubeconfig(oktetoPath, &commands.KubeconfigOpts{
		OktetoHome: dir,
	}))
	c, _, err := okteto.NewK8sClientProvider().Provide(kubeconfig.Get([]string{filepath.Join(dir, ".kube", "config")}))
	require.NoError(t, err)

	// Test that image is not built before running okteto deploy
	appImageDev := fmt.Sprintf("%s/%s/%s-app:okteto", okteto.GetContext().Registry, testNamespace, filepath.Base(dir))
	require.False(t, isImageBuilt(appImageDev))

	deployOptions := &commands.DeployOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
		Build:      false,
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	// Test that image is built after running okteto deploy
	require.True(t, isImageBuilt(appImageDev))

	// Test that endpoint works
	autowakeURL := fmt.Sprintf("https://e2etest-%s.%s", testNamespace, appsSubdomain)
	require.NotEmpty(t, integration.GetContentFromURL(autowakeURL, timeout))

	deployOptions.LogLevel = "debug"
	output, err := commands.RunOktetoDeployAndGetOutput(oktetoPath, deployOptions)
	require.NoError(t, err)

	err = expectImageFoundNoSkippingBuild(output)
	log.Print(output)
	require.Error(t, err, err)

	_, err = integration.GetConfigmap(context.Background(), testNamespace, fmt.Sprintf("okteto-git-%s", filepath.Base(dir)), c)
	require.NoError(t, err)

	destroyOptions := &commands.DestroyOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
	}
	require.NoError(t, commands.RunOktetoDestroy(oktetoPath, destroyOptions))

	_, err = integration.GetService(context.Background(), testNamespace, "e2etest", c)
	require.True(t, k8sErrors.IsNotFound(err))
	require.NoError(t, commands.RunOktetoDeleteNamespace(oktetoPath, namespaceOpts))
}

// TestDeployOktetoManifestExportCache tests the following scenario:
// - Deploying a okteto manifest locally with a build that has a export cache
func TestDeployOktetoManifestExportCache(t *testing.T) {
	t.Parallel()
	oktetoPath, err := integration.GetOktetoPath()
	require.NoError(t, err)

	dir := t.TempDir()

	testNamespace := integration.GetTestNamespace(t.Name())
	namespaceOpts := &commands.NamespaceOptions{
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoCreateNamespace(oktetoPath, namespaceOpts))
	require.NoError(t, commands.RunOktetoKubeconfig(oktetoPath, &commands.KubeconfigOpts{
		OktetoHome: dir,
	}))
	c, _, err := okteto.NewK8sClientProvider().Provide(kubeconfig.Get([]string{filepath.Join(dir, ".kube", "config")}))
	require.NoError(t, err)

	require.NoError(t, createOktetoManifestWithCache(dir))
	require.NoError(t, createAppDockerfileWithCache(dir))
	appImageDev := fmt.Sprintf("%s/%s/app:dev", okteto.GetContext().Registry, testNamespace)
	require.NoError(t, createK8sManifestWithCache(dir, appImageDev))

	deployOptions := &commands.DeployOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	// Test that image has been built
	require.NotEmpty(t, getImageWithSHA(fmt.Sprintf("%s/%s/app:dev", okteto.GetContext().Registry, testNamespace)))

	destroyOptions := &commands.DestroyOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
	}
	require.NoError(t, commands.RunOktetoDestroy(oktetoPath, destroyOptions))

	_, err = integration.GetService(context.Background(), testNamespace, "e2etest", c)
	require.True(t, k8sErrors.IsNotFound(err))
	require.NoError(t, commands.RunOktetoDeleteNamespace(oktetoPath, namespaceOpts))
}

// TestDeployRemoteOktetoManifest tests the following scenario:
// - Deploying a okteto manifest in remote with a build locally
func TestDeployRemoteOktetoManifest(t *testing.T) {
	oktetoPath, err := integration.GetOktetoPath()
	require.NoError(t, err)

	dir := t.TempDir()

	testNamespace := integration.GetTestNamespace(t.Name())
	namespaceOpts := &commands.NamespaceOptions{
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoCreateNamespace(oktetoPath, namespaceOpts))
	require.NoError(t, commands.RunOktetoKubeconfig(oktetoPath, &commands.KubeconfigOpts{
		OktetoHome: dir,
	}))
	c, _, err := okteto.NewK8sClientProvider().Provide(kubeconfig.Get([]string{filepath.Join(dir, ".kube", "config")}))
	require.NoError(t, err)

	require.NoError(t, createOktetoManifestWithDeployRemote(dir))
	require.NoError(t, createAppDockerfileWithCache(dir))

	buildOptions := &commands.BuildOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
	}

	require.NoError(t, commands.RunOktetoBuild(oktetoPath, buildOptions))

	// Test that image has been built
	require.NotEmpty(t, getImageWithSHA(fmt.Sprintf("%s/%s/app:dev", okteto.GetContext().Registry, testNamespace)))

	deployOptions := &commands.DeployOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	destroyOptions := &commands.DestroyOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
	}
	require.NoError(t, commands.RunOktetoDestroyRemote(oktetoPath, destroyOptions))

	_, err = integration.GetDeployment(context.Background(), testNamespace, "my-dep", c)
	require.True(t, k8sErrors.IsNotFound(err))
	require.NoError(t, commands.RunOktetoDeleteNamespace(oktetoPath, namespaceOpts))
}

// TestDeployRemoteOktetoManifest tests the following scenario:
// - Deploying a okteto manifest in remote with a build locally
func TestDeployRemoteOktetoManifestFromParentFolder(t *testing.T) {
	t.Parallel()
	oktetoPath, err := integration.GetOktetoPath()
	require.NoError(t, err)

	dir := t.TempDir()
	parentFolder := filepath.Join(dir, "test-parent")

	testNamespace := integration.GetTestNamespace(t.Name())
	namespaceOpts := &commands.NamespaceOptions{
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoCreateNamespace(oktetoPath, namespaceOpts))
	require.NoError(t, commands.RunOktetoKubeconfig(oktetoPath, &commands.KubeconfigOpts{
		OktetoHome: dir,
	}))
	c, _, err := okteto.NewK8sClientProvider().Provide(kubeconfig.Get([]string{filepath.Join(dir, ".kube", "config")}))
	require.NoError(t, err)

	require.NoError(t, createOktetoManifestWithDeployRemote(dir))
	require.NoError(t, createAppDockerfileWithCache(dir))
	require.NoError(t, os.Mkdir(parentFolder, 0700))

	deployOptions := &commands.DeployOptions{
		Workdir:      parentFolder,
		Namespace:    testNamespace,
		OktetoHome:   dir,
		Token:        token,
		ManifestPath: filepath.Clean("../okteto.yml"),
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	// Test that image has been built
	require.NotEmpty(t, getImageWithSHA(fmt.Sprintf("%s/%s/app:dev", okteto.GetContext().Registry, testNamespace)))

	destroyOptions := &commands.DestroyOptions{
		Workdir:      parentFolder,
		Namespace:    testNamespace,
		OktetoHome:   dir,
		ManifestPath: filepath.Clean("../okteto.yml"),
	}
	require.NoError(t, commands.RunOktetoDestroyRemote(oktetoPath, destroyOptions))

	_, err = integration.GetDeployment(context.Background(), testNamespace, "my-dep", c)
	require.True(t, k8sErrors.IsNotFound(err))
	require.NoError(t, commands.RunOktetoDeleteNamespace(oktetoPath, namespaceOpts))
}

// TestDeployOktetoManifestWithinRepository tests the following scenario:
// - Deploying a okteto manifest locally within a repository from different subdirectories to check it stores it successfully
func TestDeployOktetoManifestWithinRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	oktetoPath, err := integration.GetOktetoPath()
	require.NoError(t, err)

	dir := t.TempDir()
	subdirA := filepath.Join(dir, "subdirA")
	err = os.MkdirAll(subdirA, 0700)
	require.NoError(t, err)

	subdirB := filepath.Join(subdirA, "subdirB")
	err = os.MkdirAll(subdirB, 0700)
	require.NoError(t, err)

	expectedAppName := "e2e-deploy-test"
	repository := "https://github.com/okteto/e2e-deploy-test.git"
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	_, err = r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{repository}})
	require.NoError(t, err)

	require.NoError(t, createOktetoManifest(dir, simpleOktetoManifestContent))

	testNamespace := integration.GetTestNamespace(t.Name())
	namespaceOpts := &commands.NamespaceOptions{
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoCreateNamespace(oktetoPath, namespaceOpts))
	require.NoError(t, commands.RunOktetoKubeconfig(oktetoPath, &commands.KubeconfigOpts{
		OktetoHome: dir,
	}))
	c, _, err := okteto.NewK8sClientProvider().Provide(kubeconfig.Get([]string{filepath.Join(dir, ".kube", "config")}))
	require.NoError(t, err)

	// Execute "okteto deploy" from the root of the repository
	deployOptions := &commands.DeployOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
		Token:      token,
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	cfg, err := c.CoreV1().ConfigMaps(testNamespace).Get(ctx, pipeline.TranslatePipelineName(expectedAppName), metav1.GetOptions{})
	require.NoError(t, err)

	filename := cfg.Data["filename"]
	require.Equal(t, "", filename)

	require.NoError(t, createOktetoManifest(subdirA, simpleOktetoManifestContent))

	// Execute "okteto deploy -f subdirA/okteto.yml" from root of the repo
	deployOptions = &commands.DeployOptions{
		Workdir:      dir,
		Namespace:    testNamespace,
		OktetoHome:   dir,
		Token:        token,
		ManifestPath: filepath.Join("subdirA", "okteto.yml"),
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	cfg, err = c.CoreV1().ConfigMaps(testNamespace).Get(ctx, pipeline.TranslatePipelineName(expectedAppName), metav1.GetOptions{})
	require.NoError(t, err)

	filename = cfg.Data["filename"]
	require.Equal(t, filename, filepath.Join("subdirA", "okteto.yml"))

	require.NoError(t, createOktetoManifest(subdirB, simpleOktetoManifestContent))

	// Execute "okteto deploy -f subdirA/subdirB/okteto.yml" from root of the repo
	deployOptions = &commands.DeployOptions{
		Workdir:      dir,
		Namespace:    testNamespace,
		OktetoHome:   dir,
		Token:        token,
		ManifestPath: filepath.Join("subdirA", "subdirB", "okteto.yml"),
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	cfg, err = c.CoreV1().ConfigMaps(testNamespace).Get(ctx, pipeline.TranslatePipelineName(expectedAppName), metav1.GetOptions{})
	require.NoError(t, err)

	filename = cfg.Data["filename"]
	require.Equal(t, filename, filepath.Join("subdirA", "subdirB", "okteto.yml"))

	// Execute "okteto deploy -f subdirB/okteto.yml" from subdirA
	deployOptions = &commands.DeployOptions{
		Workdir:      subdirA,
		Namespace:    testNamespace,
		OktetoHome:   dir,
		Token:        token,
		ManifestPath: filepath.Join("subdirB", "okteto.yml"),
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	cfg, err = c.CoreV1().ConfigMaps(testNamespace).Get(ctx, pipeline.TranslatePipelineName(expectedAppName), metav1.GetOptions{})
	require.NoError(t, err)

	filename = cfg.Data["filename"]
	require.Equal(t, filename, filepath.Join("subdirA", "subdirB", "okteto.yml"))

	// Execute "okteto deploy -f ../../okteto.yml" from subdirB
	deployOptions = &commands.DeployOptions{
		Workdir:      subdirB,
		Namespace:    testNamespace,
		OktetoHome:   dir,
		Token:        token,
		ManifestPath: filepath.Join("..", "..", "okteto.yml"),
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	cfg, err = c.CoreV1().ConfigMaps(testNamespace).Get(ctx, pipeline.TranslatePipelineName(expectedAppName), metav1.GetOptions{})
	require.NoError(t, err)

	filename = cfg.Data["filename"]
	require.Equal(t, filename, filepath.Join("okteto.yml"))

	// Execute "okteto deploy -f subdirA/subdirB/okteto.yml" from root of the repo
	deployOptions = &commands.DeployOptions{
		Workdir:      dir,
		Namespace:    testNamespace,
		OktetoHome:   dir,
		Token:        token,
		ManifestPath: filepath.Join("subdirA", "subdirB", "okteto.yml"),
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	cfg, err = c.CoreV1().ConfigMaps(testNamespace).Get(ctx, pipeline.TranslatePipelineName(expectedAppName), metav1.GetOptions{})
	require.NoError(t, err)

	filename = cfg.Data["filename"]
	require.Equal(t, filename, filepath.Join("subdirA", "subdirB", "okteto.yml"))

	// Execute "okteto deploy -f <root>/subdirA/subdirB/okteto.yml" from outside of the repo
	deployOptions = &commands.DeployOptions{
		Workdir:      filepath.Dir(dir),
		Namespace:    testNamespace,
		OktetoHome:   dir,
		Token:        token,
		ManifestPath: filepath.Join(filepath.Base(dir), "subdirA", "subdirB", "okteto.yml"),
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	cfg, err = c.CoreV1().ConfigMaps(testNamespace).Get(ctx, pipeline.TranslatePipelineName(expectedAppName), metav1.GetOptions{})
	require.NoError(t, err)

	filename = cfg.Data["filename"]
	require.Equal(t, filename, filepath.Join("subdirA", "subdirB", "okteto.yml"))

	// Execute "okteto deploy -f <absolute-path>/subdirA/subdirB/okteto.yml" from the repo
	deployOptions = &commands.DeployOptions{
		Workdir:      dir,
		Namespace:    testNamespace,
		OktetoHome:   dir,
		Token:        token,
		ManifestPath: filepath.Join(subdirB, "okteto.yml"),
	}
	require.NoError(t, commands.RunOktetoDeploy(oktetoPath, deployOptions))

	cfg, err = c.CoreV1().ConfigMaps(testNamespace).Get(ctx, pipeline.TranslatePipelineName(expectedAppName), metav1.GetOptions{})
	require.NoError(t, err)

	filename = cfg.Data["filename"]
	require.Equal(t, filename, filepath.Join("subdirA", "subdirB", "okteto.yml"))

	destroyOptions := &commands.DestroyOptions{
		Workdir:    dir,
		Namespace:  testNamespace,
		OktetoHome: dir,
	}
	require.NoError(t, commands.RunOktetoDestroy(oktetoPath, destroyOptions))

	require.NoError(t, commands.RunOktetoDeleteNamespace(oktetoPath, namespaceOpts))
}

func isImageBuilt(image string) bool {
	reg := registry.NewOktetoRegistry(okteto.Config{})
	if _, err := reg.GetImageTagWithDigest(image); err == nil {
		return true
	}
	return false
}

func createOktetoManifestWithDeployRemote(dir string) error {
	dockerfilePath := filepath.Join(dir, oktetoManifestName)
	dockerfileContent := []byte(oktetoManifestWithDeployRemote)
	if err := os.WriteFile(dockerfilePath, dockerfileContent, 0600); err != nil {
		return err
	}
	return nil
}

func createOktetoManifestWithCache(dir string) error {
	dockerfilePath := filepath.Join(dir, oktetoManifestName)
	dockerfileContent := []byte(oktetoManifestContentWithCache)
	if err := os.WriteFile(dockerfilePath, dockerfileContent, 0600); err != nil {
		return err
	}
	return nil
}

func createOktetoManifest(dir, content string) error {
	manifestPath := filepath.Join(dir, oktetoManifestName)
	manifestContent := []byte(content)
	if err := os.WriteFile(manifestPath, manifestContent, 0600); err != nil {
		return err
	}
	return nil
}

func createOktetoManifestWithName(dir, content, name string) error {
	manifestPath := filepath.Join(dir, name)
	manifestContent := []byte(content)
	if err := os.WriteFile(manifestPath, manifestContent, 0600); err != nil {
		return err
	}
	return nil
}

func expectImageFoundNoSkippingBuild(output string) error {
	if ok := strings.Contains(output, "Skipping build for image for service"); ok {
		return errors.New("expected image found, skipping build")
	}
	return nil
}

func expectForceBuild(output string) error {
	if ok := strings.Contains(output, "force build from manifest definition"); !ok {
		return errors.New("expected force build from manifest definition")
	}
	return nil
}

func createK8sManifestWithCache(dir, image string) error {
	dockerfilePath := filepath.Join(dir, k8sManifestName)

	dockerfileContent := []byte(fmt.Sprintf(k8sManifestTemplateWithCache, image))
	if err := os.WriteFile(dockerfilePath, dockerfileContent, 0600); err != nil {
		return err
	}
	return nil
}

func createAppDockerfileWithCache(dir string) error {
	if err := os.Mkdir(filepath.Join(dir, "app"), 0700); err != nil {
		return err
	}

	appDockerfilePath := filepath.Join(dir, "app", "Dockerfile")
	appDockerfileContent := []byte(appDockerfileWithCache)
	if err := os.WriteFile(appDockerfilePath, appDockerfileContent, 0600); err != nil {
		return err
	}
	return nil
}
