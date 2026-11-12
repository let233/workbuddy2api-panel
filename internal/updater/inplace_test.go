package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestInstallSupportMatrix 锁住「这台部署能不能原地更新」的判定矩阵。
//
// 历史实现里容器一律判为不支持（helper 协议要求父进程先退出），本次改成容器内走
// in-place，于是「容器 + 可写目录」必须是可装、「容器 + 只读根文件系统」必须是
// 明确的一句原因——面板把这句话直接展示给用户。
func TestInstallSupportMatrix(t *testing.T) {
	const v = "2026.10.22.1823"
	cases := []struct {
		name         string
		version      string
		goos, goarch string
		executable   string
		container    bool
		writable     bool
		supervised   bool
		want         bool
		wantReason   string
	}{
		{"native writable", v, "linux", "amd64", "/app/wb2api", false, true, false, true, ""},
		{"native readonly", v, "linux", "amd64", "/app/wb2api", false, false, false, false, "程序目录不可写"},
		{"native supervised", v, "linux", "amd64", "/app/wb2api", false, true, true, false, "系统服务托管"},
		{"container writable", v, "linux", "amd64", "/app/wb2api", true, true, false, true, ""},
		{"container readonly", v, "linux", "amd64", "/app/wb2api", true, false, false, false, "容器内程序目录不可写"},
		{"container ignores supervisor", v, "linux", "arm64", "/app/wb2api", true, true, true, true, ""},
		{"container windows", v, "windows", "amd64", "/app/wb2api.exe", true, true, false, false, "Windows 容器"},
		{"dev build", "dev", "linux", "amd64", "/app/wb2api", true, true, false, false, "不是时间版本"},
		{"missing executable", v, "linux", "amd64", "", true, true, false, false, "定位不到"},
		{"unsupported os", v, "freebsd", "amd64", "/app/wb2api", false, true, false, false, "当前系统"},
		{"unsupported arch", v, "linux", "386", "/app/wb2api", false, true, false, false, "当前架构"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := installSupport(c.version, c.goos, c.goarch, c.executable, c.container, c.writable, c.supervised)
			if got != c.want {
				t.Fatalf("installSupport = %v, want %v (reason %q)", got, c.want, reason)
			}
			if !strings.Contains(reason, c.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", reason, c.wantReason)
			}
		})
	}
}

// testArchive 造一个只含单个可执行文件的 Release 压缩包。
func testArchive(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testReleaseTransport 模拟 api.github.com 的 Release 列表与资产下载。
func testReleaseTransport(t *testing.T, archive []byte, version string) roundTripFunc {
	t.Helper()
	sum := sha256.Sum256(archive)
	return func(r *http.Request) (*http.Response, error) {
		body := archive
		if r.URL.Host == "api.github.com" {
			payload, err := json.Marshal([]release{{
				TagName: "v" + version,
				Assets: []releaseAsset{{
					Name:               fmt.Sprintf("wb2api-panel-%s-%s-%s.zip", version, runtime.GOOS, runtime.GOARCH),
					BrowserDownloadURL: "https://github.com/example/project/releases/download/test/file.zip",
					Size:               int64(len(archive)),
					Digest:             "sha256:" + hex.EncodeToString(sum[:]),
				}},
			}})
			if err != nil {
				return nil, err
			}
			body = payload
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)), Header: make(http.Header)}, nil
	}
}

// TestContainerInstallReplacesInPlaceAndExecs 容器内安装：不拉 helper、直接换文件，
// 由 RestartAfter 触发的 exec 接管进程（测试里注入 reexec 观察调用）。
func TestContainerInstallReplacesInPlaceAndExecs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("容器内 in-place 更新只在 linux 上可达")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "wb2api")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, "wb2api", []byte("new-binary"))

	m := New("2026.10.22.1823", "example/project", "", Settings{})
	m.container = true
	m.executable = target
	m.execDirWritable = true
	m.client.Transport = testReleaseTransport(t, archive, "2026.10.23.1859")

	executed := make(chan []string, 1)
	var once sync.Once
	m.reexec = func(path string, argv []string) error {
		once.Do(func() { executed <- append([]string{path}, argv...) })
		return nil
	}

	if !m.Status().CanInstall {
		t.Fatalf("container install must be available: %+v", m.Status())
	}
	if err := m.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new-binary" {
		t.Fatalf("binary not replaced: %q %v", got, err)
	}
	backup, err := os.ReadFile(target + ".old")
	if err != nil || string(backup) != "old-binary" {
		t.Fatalf("backup missing or wrong: %q %v", backup, err)
	}
	if _, err := os.Stat(filepath.Join(dir, replacedMarkerName)); err != nil {
		t.Fatalf("replaced marker missing: %v", err)
	}
	status := m.Status()
	if !status.Installing || status.InstallMode != "in-place" || status.LastError != "" {
		t.Fatalf("unexpected status after install: %+v", status)
	}

	m.RestartAfter(time.Millisecond)
	select {
	case call := <-executed:
		if call[0] != target {
			t.Fatalf("exec path = %q, want %q", call[0], target)
		}
		if len(call) < 2 || call[1] != os.Args[0] {
			t.Fatalf("exec argv must keep the original arguments: %v", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("container restart did not exec the new binary")
	}
}

// TestInPlaceFinalizeRemovesBackup 新版本启动后自证：标记版本对得上才删 .old，
// 对不上（exec 失败、旧版本还在跑）必须保留回滚点。
func TestInPlaceFinalizeRemovesBackup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WB2API_UPDATE_FIXTURE", dir)
	t.Setenv("WB2API_UPDATE_GRACE_MS", "20")
	target := filepath.Join(dir, "wb2api")
	if err := os.WriteFile(target, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".old", []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, replacedMarkerName)
	if err := writeReplacedMarker(dir, "2026.10.23.1859"); err != nil {
		t.Fatal(err)
	}

	m := New("2026.10.22.1823", "example/project", "", Settings{})
	m.executable = target
	m.armInPlaceFinalize()
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(target + ".old"); err != nil {
		t.Fatal("mismatched marker must keep the rollback backup")
	}

	if err := writeReplacedMarker(dir, "2026.10.23.1859"); err != nil {
		t.Fatal(err)
	}
	m.version = "2026.10.23.1859"
	m.armInPlaceFinalize()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, backupErr := os.Stat(target + ".old")
		_, markerErr := os.Stat(marker)
		if os.IsNotExist(backupErr) && os.IsNotExist(markerErr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup/marker not cleaned: backup=%v marker=%v", backupErr, markerErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestContainerInstallReportsReadOnlyDir 只读程序目录不许装，且原因能直达面板。
func TestContainerInstallReportsReadOnlyDir(t *testing.T) {
	m := New("2026.10.22.1823", "example/project", "", Settings{})
	m.container = true
	m.executable = filepath.Join(t.TempDir(), "wb2api")
	m.execDirWritable = false
	m.refreshInstallStatusLocked()
	status := m.Status()
	if status.CanInstall || status.InstallBlocked == "" || status.InstallMode != "" {
		t.Fatalf("read-only container dir must be blocked with a reason: %+v", status)
	}
	want := "容器内程序目录不可写"
	if runtime.GOOS == "windows" {
		// Windows 上没有 exec 原语，容器分支先被「Windows 容器不支持」挡下；这里
		// 只验证「容器专属原因能一路传到调用方」，矩阵用例里已逐档覆盖判定本身。
		want = "Windows 容器不支持在线更新"
	}
	err := m.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("install must fail with the container reason, got %v", err)
	}
}
