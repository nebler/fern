package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestExportManagedVolumesRetainsVolumesAndRemovesHelpers(t *testing.T) {
	t.Parallel()
	spec := ownershipTestSpec()
	spec.WorkspaceGH = true
	contents := map[string]string{
		specDataVolumeName(spec): "session-data\n",
		specGHVolumeName(spec):   "oauth_token: secret\n",
	}
	var lock sync.Mutex
	helpers := make(map[string]string)
	created, removed := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		switch {
		case request.Method == http.MethodGet && strings.Contains(path, "/volumes/"):
			name := path[strings.LastIndex(path, "/")+1:]
			writeJSON(writer, http.StatusOK, map[string]any{"Name": name, "Labels": map[string]string{
				managedLabel: labelTrue, workspaceLabel: spec.Name,
			}})
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/containers/create"):
			var body struct {
				HostConfig struct {
					Mounts []struct{ Source string }
				}
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || len(body.HostConfig.Mounts) != 1 {
				t.Errorf("decode helper create: mounts=%+v err=%v", body.HostConfig.Mounts, err)
			}
			lock.Lock()
			created++
			id := "helper-" + body.HostConfig.Mounts[0].Source
			helpers[id] = body.HostConfig.Mounts[0].Source
			lock.Unlock()
			writeJSON(writer, http.StatusCreated, map[string]any{"Id": id})
		case request.Method == http.MethodGet && strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/archive"):
			parts := strings.Split(path, "/")
			id := parts[len(parts)-2]
			lock.Lock()
			name := helpers[id]
			lock.Unlock()
			stat, _ := json.Marshal(map[string]any{"name": ".", "size": 0, "mode": os.ModeDir | 0o700})
			writer.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
			archive := tar.NewWriter(writer)
			filename := "sessions.db"
			if name == specGHVolumeName(spec) {
				filename = "hosts.yml"
			}
			data := []byte(contents[name])
			_ = archive.WriteHeader(&tar.Header{Name: filename, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg})
			_, _ = archive.Write(data)
			_ = archive.Close()
		case request.Method == http.MethodDelete && strings.Contains(path, "/containers/"):
			lock.Lock()
			removed++
			lock.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && strings.Contains(path, "/volumes/"):
			t.Errorf("export removed durable volume: %s", path)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	destination := t.TempDir()
	names, err := testDocker(t, server).ExportManagedVolumes(context.Background(), spec, destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != specDataVolumeName(spec) || names[1] != specGHVolumeName(spec) {
		t.Fatalf("volume names = %q", names)
	}
	data, err := os.ReadFile(filepath.Join(destination, specGHVolumeName(spec), "hosts.yml"))
	if err != nil || string(data) != contents[specGHVolumeName(spec)] {
		t.Fatalf("GitHub volume export = %q, %v", data, err)
	}
	if created != 2 || removed != 2 {
		t.Fatalf("helper creates/removes = %d/%d", created, removed)
	}
}

func TestExtractDockerArchiveRejectsTamperEntries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, path string
		typeflag   byte
	}{
		{name: "escape", path: "../escape", typeflag: tar.TypeReg},
		{name: "symlink", path: "link", typeflag: tar.TypeSymlink},
		{name: "hardlink", path: "link", typeflag: tar.TypeLink},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := tar.NewWriter(&buffer)
			if err := writer.WriteHeader(&tar.Header{Name: test.path, Typeflag: test.typeflag, Linkname: "/etc/passwd"}); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractDockerArchive(bytes.NewReader(buffer.Bytes()), t.TempDir()); err == nil {
				t.Fatal("unsafe Docker archive was accepted")
			}
		})
	}
}

func TestRestoreManagedVolumesStagesVerifiesAndReplacesCanonicalVolumes(t *testing.T) {
	t.Parallel()
	spec := ownershipTestSpec()
	spec.WorkspaceGH = true
	type fakeVolume struct {
		labels map[string]string
		files  map[string][]byte
	}
	volumes := map[string]*fakeVolume{
		specDataVolumeName(spec): {labels: map[string]string{managedLabel: labelTrue, workspaceLabel: spec.Name}, files: map[string][]byte{"old": []byte("old-data")}},
		specGHVolumeName(spec):   {labels: map[string]string{managedLabel: labelTrue, workspaceLabel: spec.Name}, files: map[string][]byte{"hosts.yml": []byte("old-token")}},
	}
	helpers := make(map[string]string)
	var lock sync.Mutex
	nextHelper := 0
	failCanonicalImport := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		switch {
		case request.Method == http.MethodGet && strings.Contains(path, "/volumes/"):
			name := path[strings.LastIndex(path, "/")+1:]
			lock.Lock()
			value := volumes[name]
			lock.Unlock()
			if value == nil {
				writeDockerNotFound(writer, "volume")
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"Name": name, "Labels": value.labels})
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/volumes/create"):
			var options struct {
				Name   string
				Labels map[string]string
			}
			if err := json.NewDecoder(request.Body).Decode(&options); err != nil {
				t.Errorf("decode volume create: %v", err)
			}
			lock.Lock()
			if volumes[options.Name] == nil {
				volumes[options.Name] = &fakeVolume{labels: options.Labels, files: make(map[string][]byte)}
			}
			value := volumes[options.Name]
			lock.Unlock()
			writeJSON(writer, http.StatusCreated, map[string]any{"Name": options.Name, "Labels": value.labels})
		case request.Method == http.MethodDelete && strings.Contains(path, "/volumes/"):
			name := path[strings.LastIndex(path, "/")+1:]
			lock.Lock()
			delete(volumes, name)
			lock.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/containers/create"):
			var body struct {
				HostConfig struct {
					Mounts []struct{ Source string }
				}
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || len(body.HostConfig.Mounts) != 1 {
				t.Errorf("decode helper create: mounts=%+v err=%v", body.HostConfig.Mounts, err)
			}
			lock.Lock()
			nextHelper++
			id := fmt.Sprintf("helper-%d", nextHelper)
			helpers[id] = body.HostConfig.Mounts[0].Source
			lock.Unlock()
			writeJSON(writer, http.StatusCreated, map[string]any{"Id": id})
		case request.Method == http.MethodGet && strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/archive"):
			parts := strings.Split(path, "/")
			id := parts[len(parts)-2]
			lock.Lock()
			value := volumes[helpers[id]]
			files := make(map[string][]byte, len(value.files))
			for name, data := range value.files {
				files[name] = append([]byte(nil), data...)
			}
			lock.Unlock()
			stat, _ := json.Marshal(map[string]any{"name": ".", "size": 0, "mode": os.ModeDir | 0o700})
			writer.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
			archive := tar.NewWriter(writer)
			for name, data := range files {
				_ = archive.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg})
				_, _ = archive.Write(data)
			}
			_ = archive.Close()
		case request.Method == http.MethodPut && strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/archive"):
			parts := strings.Split(path, "/")
			id := parts[len(parts)-2]
			lock.Lock()
			target := helpers[id]
			if failCanonicalImport && target == specGHVolumeName(spec) {
				failCanonicalImport = false
				lock.Unlock()
				http.Error(writer, "injected canonical import failure", http.StatusInternalServerError)
				return
			}
			lock.Unlock()
			files := make(map[string][]byte)
			archive := tar.NewReader(request.Body)
			for {
				header, err := archive.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Errorf("read imported archive: %v", err)
					break
				}
				if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
					files[header.Name], _ = io.ReadAll(archive)
				}
			}
			lock.Lock()
			volumes[helpers[id]].files = files
			lock.Unlock()
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodDelete && strings.Contains(path, "/containers/"):
			parts := strings.Split(path, "/")
			id := parts[len(parts)-1]
			lock.Lock()
			delete(helpers, id)
			lock.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	sources := make(map[string]string)
	for name, value := range map[string]string{specDataVolumeName(spec): "new-data", specGHVolumeName(spec): "new-token"} {
		directory := t.TempDir()
		filename := "state"
		if name == specGHVolumeName(spec) {
			filename = "hosts.yml"
		}
		if err := os.WriteFile(filepath.Join(directory, filename), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		sources[name] = directory
	}
	if err := testDocker(t, server).RestoreManagedVolumes(context.Background(), spec, sources, "generation-a"); err != nil {
		t.Fatal(err)
	}
	docker := testDocker(t, server)
	firstRotation := credentialTestArchive(t, map[string]string{"hosts.yml": "rotated-token"})
	if err := docker.ReplaceWorkspaceGH(context.Background(), spec, firstRotation, "rotation-a"); err != nil {
		t.Fatal(err)
	}
	lock.Lock()
	failCanonicalImport = true
	lock.Unlock()
	failedRotation := credentialTestArchive(t, map[string]string{"hosts.yml": "must-not-stick"})
	if err := docker.ReplaceWorkspaceGH(context.Background(), spec, failedRotation, "rotation-b"); err == nil {
		t.Fatal("workspace-gh replacement accepted an activation failure")
	}
	lock.Lock()
	defer lock.Unlock()
	if len(volumes) != 2 || len(helpers) != 0 {
		t.Fatalf("volume count=%d helper count=%d", len(volumes), len(helpers))
	}
	if string(volumes[specDataVolumeName(spec)].files["state"]) != "new-data" || string(volumes[specGHVolumeName(spec)].files["hosts.yml"]) != "rotated-token" {
		t.Fatalf("restored volumes = %+v", volumes)
	}
}

func TestCredentialArchiveRejectsUnsafeCandidates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		typeflag byte
		mode     int64
	}{
		{name: "public permissions", typeflag: tar.TypeReg, mode: 0o644},
		{name: "executable", typeflag: tar.TypeReg, mode: 0o700},
		{name: "symlink", typeflag: tar.TypeSymlink, mode: 0o600},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := tar.NewWriter(&buffer)
			if err := writer.WriteHeader(&tar.Header{Name: "hosts.yml", Typeflag: test.typeflag, Mode: test.mode, Size: 1, Linkname: "/etc/passwd"}); err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write([]byte("x"))
			_ = writer.Close()
			if _, err := credentialArchiveInventory(buffer.Bytes()); err == nil {
				t.Fatal("unsafe credential archive was accepted")
			}
		})
	}
}

func credentialTestArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, value := range files {
		data := []byte(value)
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestArchiveDirectoryAndExtractRoundTrip(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "state"), []byte("durable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := archiveDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	var data bytes.Buffer
	if _, err := io.Copy(&data, archive); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := extractDockerArchive(bytes.NewReader(data.Bytes()), destination); err != nil {
		t.Fatal(err)
	}
	if err := compareTrees(source, destination); err != nil {
		t.Fatal(err)
	}
}
