package updater

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// 更新包里的「非二进制部分」：前端产物 public/ 与随包文档。
//
// 历史实现只换二进制（extractBinary 里 file.Name != expectedName 就 continue），
// 于是 public/ 永远停在第一次手工解压那一版：面板说「已是最新版本」，浏览器里跑
// 的还是旧前端，页面上的新功能一个都不出现。现在整包落地：
//
//  1. Install 把 public/ 与文档解到程序目录旁的 .wb2api-payload-* 暂存目录；
//  2. helper 等旧进程退出后先换二进制，再把暂存目录整体换到运行目录（public 走
//     整个目录的 rename 交换，不会出现「新旧文件混装」）；
//  3. 新版本没活过观察窗口 → 二进制与 public/ 一起回滚。
const (
	// payloadDirPrefix 暂存目录前缀。必须是 .wb2api-：helper 只认这种名字
	// （validateHelperPath），清理进程也只删这种残留。
	payloadDirPrefix = ".wb2api-payload-"
	// payloadFileLimit 单个文件上限：前端产物都是小文件，超过这个量级说明包不对。
	payloadFileLimit = 128 << 20
	// payloadTotalLimit 整包非二进制部分的上限。
	payloadTotalLimit = 400 << 20
	// publicDirName 包内前端产物目录名（vite build 的输出目录）。
	publicDirName = "public"
	// payloadIndexFile 判断「这个包到底带没带前端」的锚点文件。
	payloadIndexFile = "index.html"
	// payloadBackupSuffix 安装时旧目录/旧文档的备份后缀（成功健康检查后删除）。
	payloadBackupSuffix = ".wb2api-old"
)

// payloadDocs 随包一起换掉的文档（不含用户自己的 config.json）。
var payloadDocs = []string{"config.example.json", "README.md"}

// extractPayload 把更新包里的前端产物与随包文档解到 destDir。
//
// 返回 found 表示包里到底有没有 public/index.html：老包（或第三方重打包）没有前端
// 产物时不能当成错误——二进制照样该更新，只是前端保持现状并由调用方告警。
func extractPayload(archivePath, destDir string) (found bool, err error) {
	if strings.TrimSpace(destDir) == "" {
		return false, errors.New("更新暂存目录为空")
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return false, fmt.Errorf("读取更新包: %w", err)
	}
	defer zr.Close()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return false, fmt.Errorf("创建更新暂存目录: %w", err)
	}
	var (
		total   int64
		written int
	)
	for _, file := range zr.File {
		name := strings.TrimPrefix(filepath.ToSlash(file.Name), "./")
		if name == "" || strings.HasSuffix(name, "/") || file.FileInfo().IsDir() {
			continue // 目录条目：按需现建
		}
		rel, wanted := payloadTarget(name)
		if !wanted {
			continue // 二进制与其它未知条目由 extractBinary 或直接忽略
		}
		name = rel
		if err := safePayloadName(name); err != nil {
			return false, err
		}
		if file.UncompressedSize64 > payloadFileLimit {
			return false, fmt.Errorf("更新包内 %s 过大: %d 字节", name, file.UncompressedSize64)
		}
		total += int64(file.UncompressedSize64)
		if total > payloadTotalLimit {
			return false, fmt.Errorf("更新包内的前端产物超过上限 %d 字节", int64(payloadTotalLimit))
		}
		dst := filepath.Join(destDir, filepath.FromSlash(name))
		n, werr := writeZipFile(file, dst)
		if werr != nil {
			return false, werr
		}
		if uint64(n) != file.UncompressedSize64 {
			return false, fmt.Errorf("解压 %s 大小不符: %d != %d", name, n, file.UncompressedSize64)
		}
		written++
		if name == path.Join(publicDirName, payloadIndexFile) {
			found = true
		}
	}
	if written == 0 {
		log.Printf("WARN: [updater] 更新包内没有 %s/ 与随包文档，只更新程序文件", publicDirName)
	}
	return found, nil
}

// payloadTarget 把包内条目映射成「相对落地位置」，不是白名单内的条目返回 false。
//
// 除了 CI 那种平铺包（public/... 在顶层），这里也容忍整体多套一层目录的包
// （第三方重打包常见：wb2api-panel-vX-windows-amd64/public/...）——否则那种包的
// 前端产物会被当成未知条目丢掉，又回到「二进制新了、页面还是旧的」。
func payloadTarget(name string) (string, bool) {
	if payloadWanted(name) {
		return name, true
	}
	if i := strings.Index(name, "/"); i > 0 {
		if rest := name[i+1:]; payloadWanted(rest) {
			return rest, true
		}
	}
	return "", false
}

// payloadWanted 只认前端产物目录与随包文档这两个白名单，别的条目一律不落地。
func payloadWanted(name string) bool {
	if name == publicDirName || strings.HasPrefix(name, publicDirName+"/") {
		return true
	}
	for _, doc := range payloadDocs {
		if name == doc {
			return true
		}
	}
	return false
}

// safePayloadName 挡 zip 路径穿越（zip-slip）：包内写 ../ 或绝对路径就能落到程序
// 目录之外，甚至覆盖系统文件。
func safePayloadName(name string) error {
	if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, ":") {
		return fmt.Errorf("更新包含有可疑路径: %s", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("更新包含有越界路径: %s", name)
		}
	}
	return nil
}

func writeZipFile(file *zip.File, dst string) (int64, error) {
	rc, err := file.Open()
	if err != nil {
		return 0, fmt.Errorf("打开更新包内 %s: %w", file.Name, err)
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("创建目录 %s: %w", filepath.Dir(dst), err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("写出 %s: %w", dst, err)
	}
	n, cpErr := io.Copy(out, io.LimitReader(rc, payloadFileLimit+1))
	closeErr := out.Close()
	if cpErr != nil {
		return n, fmt.Errorf("写出 %s: %w", dst, cpErr)
	}
	if closeErr != nil {
		return n, fmt.Errorf("写出 %s: %w", dst, closeErr)
	}
	return n, nil
}

// installPayload 用暂存目录里的 public/ 与文档替换运行位置上的同名内容。
//
// public 走「整目录 rename 交换」而不是逐个文件覆盖：vite 产物是 contenthash 命名
// 的，新版本删掉的旧 chunk 如果留在目录里并不会坏事，但反过来——覆盖式写入遇到
// 文件被占用（Windows 上是常态）就会半新半旧，页面加载出 404 的 chunk。
//
// 返回的 commit/restore 由调用方按健康检查结果择一执行；installed 表示 public
// 真的换了（没有前端产物时为 false，commit/restore 都是空操作）。
func installPayload(stageDir, targetDir, publicDir string) (commit func(), restore func(), installed bool, err error) {
	noop := func() {}
	if strings.TrimSpace(stageDir) == "" || strings.TrimSpace(publicDir) == "" {
		return noop, noop, false, nil
	}
	var (
		commits  []func()
		restores []func()
	)
	if src := filepath.Join(stageDir, publicDirName); dirExists(src) {
		backup := publicDir + payloadBackupSuffix
		// 上一轮如果崩在「已备份未回滚」中间态，这里先收掉残留，否则下面的 rename
		// 会因为目标已存在而失败。
		if err := os.RemoveAll(backup); err != nil {
			return noop, noop, false, fmt.Errorf("清理上一次的前端备份: %w", err)
		}
		hadOld := false
		if _, statErr := os.Stat(publicDir); statErr == nil {
			if err := os.Rename(publicDir, backup); err != nil {
				return noop, noop, false, fmt.Errorf("暂存旧前端目录 %s: %w", publicDir, err)
			}
			hadOld = true
		} else if !os.IsNotExist(statErr) {
			return noop, noop, false, fmt.Errorf("检查前端目录 %s: %w", publicDir, statErr)
		}
		if err := os.Rename(src, publicDir); err != nil {
			// 暂存目录在程序目录、public 可能在别的盘：跨卷 rename 会 EXDEV，退回逐文件复制。
			if cerr := copyTree(src, publicDir); cerr != nil {
				if hadOld {
					_ = os.Rename(backup, publicDir)
				}
				return noop, noop, false, fmt.Errorf("安装前端产物到 %s: %w", publicDir, cerr)
			}
			_ = os.RemoveAll(src)
		}
		installed = true
		if hadOld {
			b := backup
			commits = append(commits, func() { _ = os.RemoveAll(b) })
			restores = append(restores, func() {
				_ = os.RemoveAll(publicDir)
				if err := os.Rename(b, publicDir); err != nil {
					log.Printf("WARN: [updater] 回滚前端产物失败，旧目录仍在 %s: %v", b, err)
				}
			})
		} else {
			restores = append(restores, func() { _ = os.RemoveAll(publicDir) })
		}
		log.Printf("[updater] 前端产物已就位: %s", publicDir)
	}
	// 随包文档：先备份再写，写失败就还原。文档不影响服务，失败只告警不阻断更新。
	for _, doc := range payloadDocs {
		srcDoc := filepath.Join(stageDir, filepath.FromSlash(doc))
		if fi, statErr := os.Stat(srcDoc); statErr != nil || fi.IsDir() {
			continue
		}
		dstDoc := filepath.Join(targetDir, filepath.FromSlash(doc))
		backupDoc := dstDoc + payloadBackupSuffix
		_ = os.Remove(backupDoc)
		hadDoc := false
		if _, statErr := os.Stat(dstDoc); statErr == nil {
			if err := os.Rename(dstDoc, backupDoc); err != nil {
				log.Printf("WARN: [updater] 跳过随包文档 %s（备份失败）: %v", doc, err)
				continue
			}
			hadDoc = true
		}
		if err := copyFileMode(srcDoc, dstDoc, 0o644); err != nil {
			if hadDoc {
				_ = os.Rename(backupDoc, dstDoc)
			}
			log.Printf("WARN: [updater] 更新随包文档 %s 失败: %v", doc, err)
			continue
		}
		if !hadDoc {
			continue
		}
		d, b := dstDoc, backupDoc
		commits = append(commits, func() { _ = os.Remove(b) })
		restores = append(restores, func() {
			_ = os.Remove(d)
			if err := os.Rename(b, d); err != nil {
				log.Printf("WARN: [updater] 还原文档 %s 失败: %v", d, err)
			}
		})
	}
	return func() {
			for _, fn := range commits {
				fn()
			}
		}, func() {
			for i := len(restores) - 1; i >= 0; i-- {
				restores[i]()
			}
		}, installed, nil
}

// validatePublicDir 收敛前端产物目录参数。
//
// helper 的入参来自父进程，入口没有来源校验（见 validateHelperPath 的说明），所以
// 这里至少要挡住两类灾难：相对路径（换工作目录就写错地方）与「程序目录本身或它的
// 上级」（整目录 rename 会把安装目录搬走，等于自毁）。
func validatePublicDir(dir, targetPath string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("前端产物目录为空")
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("前端产物目录必须是绝对路径: %s", dir)
	}
	clean := filepath.Clean(dir)
	if clean == filepath.VolumeName(clean)+string(os.PathSeparator) {
		return fmt.Errorf("前端产物目录不能是盘符根目录: %s", clean)
	}
	if targetPath != "" {
		progDir := filepath.Dir(filepath.Clean(targetPath))
		if clean == progDir || strings.HasPrefix(progDir+string(os.PathSeparator), clean+string(os.PathSeparator)) {
			return fmt.Errorf("前端产物目录不能是程序目录或其上级: %s", clean)
		}
	}
	if fi, err := os.Stat(clean); err == nil && !fi.IsDir() {
		return fmt.Errorf("前端产物路径已存在且不是目录: %s", clean)
	}
	return nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// copyTree 逐文件复制目录（跨卷时 rename 的兜底）。
func copyTree(srcDir, dstDir string) error {
	return filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(srcDir, p)
		if relErr != nil {
			return relErr
		}
		dst := filepath.Join(dstDir, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFileMode(p, dst, 0o644)
	})
}

// copyFileMode 带权限的复制（updater.go 里的 copyFile 固定 0o700，是给可执行文件用
// 的，直接拿来写前端文件会得到一堆可执行位）。
func copyFileMode(srcPath, dstPath string, mode os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}
