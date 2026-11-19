package updater

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// writeZip 造一个测试用的发布包：条目名与 CI 产物一致（平铺的二进制 + 文档 + public/）。
func writeZip(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	p := filepath.Join(dir, "release.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// map 遍历无序：固定几条关键条目先写，剩下的顺序无关。
	for name, body := range files {
		w, werr := zw.Create(name)
		if werr != nil {
			t.Fatal(werr)
		}
		if _, werr := w.Write([]byte(body)); werr != nil {
			t.Fatal(werr)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// 更新包的「非二进制部分」只取白名单：public/** 与随包文档。其余（二进制、别的
// 未知条目）一律不落地——过去是整个包只认二进制，前端产物永远停在第一次解压那一版。
func TestExtractPayloadTakesOnlyFrontendAndDocs(t *testing.T) {
	dir := t.TempDir()
	archive := writeZip(t, dir, map[string]string{
		"wb2api.exe":             "binary",
		"config.example.json":    "{\"example\":true}",
		"README.md":              "# readme",
		"public/index.html":      "<html>new</html>",
		"public/assets/app-1.js": "console.log(1)",
		"notes/unrelated.txt":    "nope",
	})

	dest := filepath.Join(dir, "payload")
	found, err := extractPayload(archive, dest)
	if err != nil {
		t.Fatalf("extractPayload: %v", err)
	}
	if !found {
		t.Fatal("包里带 public/index.html，found 必须为 true")
	}
	for _, want := range []string{"public/index.html", "public/assets/app-1.js", "config.example.json", "README.md"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(want))); err != nil {
			t.Fatalf("应解出 %s: %v", want, err)
		}
	}
	for _, unwanted := range []string{"wb2api.exe", "notes/unrelated.txt"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(unwanted))); err == nil {
			t.Fatalf("白名单外的条目不该落地: %s", unwanted)
		}
	}

	// 没有 index.html 的包（老版本 / 第三方重打包）不算错误，只是 found=false。
	bare := writeZip(t, dir, map[string]string{"wb2api.exe": "binary"})
	if found, err := extractPayload(bare, filepath.Join(dir, "bare")); err != nil || found {
		t.Fatalf("found=%v err=%v，无前端产物时应为 false/nil", found, err)
	}
}

// 整包多套一层目录（第三方重打包常见）时，前端产物仍要认出来：
// wb2api-panel-vX-windows-amd64/public/index.html → public/index.html。
func TestExtractPayloadToleratesNestedRoot(t *testing.T) {
	dir := t.TempDir()
	archive := writeZip(t, dir, map[string]string{
		"wb2api-panel-v2026.9.26.711-windows-amd64/wb2api.exe":          "binary",
		"wb2api-panel-v2026.9.26.711-windows-amd64/public/index.html":   "<html>new</html>",
		"wb2api-panel-v2026.9.26.711-windows-amd64/config.example.json": "{}",
	})
	dest := filepath.Join(dir, "payload")
	found, err := extractPayload(archive, dest)
	if err != nil {
		t.Fatalf("extractPayload: %v", err)
	}
	if !found {
		t.Fatal("嵌套目录里的 public/index.html 也必须被认出来")
	}
	if _, err := os.Stat(filepath.Join(dest, "public", "index.html")); err != nil {
		t.Fatalf("应落到 public/index.html: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "config.example.json")); err != nil {
		t.Fatalf("应落到 config.example.json: %v", err)
	}
}

// zip 路径穿越必须整体失败：包内写 public/../.. 就能落到暂存目录之外。
func TestExtractPayloadRejectsZipSlip(t *testing.T) {
	dir := t.TempDir()
	archive := writeZip(t, dir, map[string]string{
		"public/../../evil.js": "boom",
		"public/index.html":    "<html></html>",
	})
	if _, err := extractPayload(archive, filepath.Join(dir, "payload")); err == nil {
		t.Fatal("含越界路径的包必须报错")
	}
}

// installPayload 用整目录 rename 交换 public/：旧目录留成 .wb2api-old，健康检查通过
// 才删（commit），不通过要能原样还原（restore）。
func TestInstallPayloadSwapAndRestore(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	publicDir := filepath.Join(dir, "public")
	target := filepath.Join(dir, "app")
	for _, d := range []string{stage, publicDir, target} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(publicDir, "index.html"), "old")
	write(filepath.Join(stage, "public", "index.html"), "new")
	write(filepath.Join(stage, "public", "assets", "a.js"), "js")
	write(filepath.Join(stage, "README.md"), "new readme")
	write(filepath.Join(target, "README.md"), "old readme")

	commit, restore, installed, err := installPayload(stage, target, publicDir)
	if err != nil {
		t.Fatalf("installPayload: %v", err)
	}
	if !installed {
		t.Fatal("包里有 public/，installed 必须为 true")
	}
	if b, _ := os.ReadFile(filepath.Join(publicDir, "index.html")); string(b) != "new" {
		t.Fatalf("public/index.html 应是新内容，得到 %q", string(b))
	}
	if _, err := os.Stat(filepath.Join(publicDir, "assets", "a.js")); err != nil {
		t.Fatalf("新增资源应一起到位: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "README.md")); string(b) != "new readme" {
		t.Fatalf("随包文档应更新，得到 %q", string(b))
	}

	// 健康检查失败：前端与文档都要回到旧内容。
	restore()
	if b, _ := os.ReadFile(filepath.Join(publicDir, "index.html")); string(b) != "old" {
		t.Fatalf("还原后 index.html 应是旧内容，得到 %q", string(b))
	}
	if _, err := os.Stat(filepath.Join(publicDir, "assets")); err == nil {
		t.Fatal("还原后不该残留新版本的资源目录")
	}
	if b, _ := os.ReadFile(filepath.Join(target, "README.md")); string(b) != "old readme" {
		t.Fatalf("还原后文档应是旧内容，得到 %q", string(b))
	}

	// 再来一次并 commit（新版本健康）：备份要被清掉，不留在磁盘上。
	write(filepath.Join(stage, "public", "index.html"), "new")
	commit2, _, _, err := installPayload(stage, target, publicDir)
	if err != nil {
		t.Fatalf("installPayload 二次: %v", err)
	}
	commit2()
	if _, err := os.Stat(publicDir + payloadBackupSuffix); err == nil {
		t.Fatalf("commit 后不该留下备份目录 %s", publicDir+payloadBackupSuffix)
	}
	_ = commit
}

// helper 入参里的 public 目录必须收敛：相对路径、盘符根、程序目录本身或它的上级
// 都会把「换前端」变成「搬走安装目录」。
func TestValidatePublicDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app", "wb2api.exe")
	if err := validatePublicDir(filepath.Join(dir, "app", "public"), target); err != nil {
		t.Fatalf("正常目录被拒: %v", err)
	}
	if err := validatePublicDir("public", target); err == nil {
		t.Fatal("相对路径必须被拒")
	}
	if err := validatePublicDir(filepath.Dir(filepath.Dir(target)), target); err == nil {
		t.Fatal("程序目录的上级必须被拒")
	}
	if err := validatePublicDir(filepath.Dir(target), target); err == nil {
		t.Fatal("程序目录本身必须被拒")
	}
}
