package updater

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// replacedMarkerName 容器内更新写下的「已替换到的版本」标记，放在程序目录里。
//
// 容器里没有 helper：换完文件就得靠 exec 自己把新版本接上来，没有第二个进程去
// 验证新版本到底起没起来。于是在**下一次**启动时自证——标记版本 == 当前运行版本，
// 说明上次替换成功，观察窗口过后删掉回滚备份；对不上说明当时 exec 没成功（旧版本
// 还在跑），备份留着供手工恢复。
const replacedMarkerName = ".wb2api-replaced"

// installSupport 判断这台部署能不能自己换二进制，并给出**不能**的原因（面板直接
// 展示，用户不用点了「立即更新」才知道不行）。
//
// 与历史实现的差别只有一处、但很关键：容器里过去一律判为「不支持」，因为换文件
// 依赖「父进程退出后由 helper 子进程替换」这套协议，而容器的主进程就是 PID 1 ——
// 一退出容器就停，helper 会被一起杀掉，替换永远走不完。容器内改用 in-place：在
// 本进程里 rename 新二进制（Linux 允许替换正在执行的 inode），再 exec 自身把同一
// 个 PID 交给新版本，端口、挂载、容器生命周期都不变。代价是没有 helper 那套
// 「新版本起不来就自动回滚」，所以旧文件留在 .old 里供手工恢复。
func installSupport(version, goos, goarch, executable string, container, dirWritable, supervised bool) (bool, string) {
	if _, ok := parseVersion(version); !ok {
		return false, "当前版本不是时间版本（dev 构建或手改版本号），无法与 Release 比较"
	}
	if goos != "linux" && goos != "windows" {
		return false, "当前系统不支持原地更新"
	}
	if goarch != "amd64" && goarch != "arm64" {
		return false, "当前架构没有对应的 Release 文件"
	}
	if executable == "" {
		return false, "定位不到正在运行的程序文件"
	}
	if container {
		if goos != "linux" {
			return false, "Windows 容器不支持在线更新"
		}
		if !dirWritable {
			return false, "容器内程序目录不可写（只读根文件系统，或 user 与目录属主不一致），请拉取新镜像"
		}
		return true, ""
	}
	if supervised {
		return false, "进程由系统服务托管（systemd/s6 等），请用服务管理器的更新流程"
	}
	if !dirWritable {
		return false, "程序目录不可写（原地更新需要写入权限）"
	}
	return true, ""
}

// dirWritable 用「建一个临时文件再删掉」判断目录能不能写。比看权限位可靠：
// 容器里属主、ACL、只读挂载混在一起，权限位看不出来。目录里的临时文件用
// .wb2api- 前缀，万一残留也会被 CleanupStaleTempFiles 扫掉。
func dirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	f, err := os.CreateTemp(dir, ".wb2api-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// installSupportLocked 是 installSupport 的当前进程快照（调用方可以持锁）。
func (m *Manager) installSupportLocked() (bool, string) {
	return installSupport(m.version, runtime.GOOS, runtime.GOARCH, m.executable, m.container, m.execDirWritable, processSupervised())
}

// refreshInstallStatusLocked 重算「能不能装、装法是什么、不能的原因」（调用方须持 m.mu）。
func (m *Manager) refreshInstallStatusLocked() {
	ok, reason := m.installSupportLocked()
	m.status.CanInstall = ok
	m.status.Container = m.container
	m.status.InstallBlocked = ""
	m.status.InstallMode = ""
	if ok {
		m.status.InstallMode = "helper"
		if m.container {
			m.status.InstallMode = "in-place"
		}
		return
	}
	m.status.InstallBlocked = reason
}

// installInPlace 容器内更新：原地替换可执行文件并把当前进程交给新版本。
//
// 调用方已把 status.Installing 置为 true、stage 文件已就绪（0755）；真正重启发生在
// RestartAfter → execSelf（handler 先回 202，再延迟 800ms）。
func (m *Manager) installInPlace(stagePath string, version parsedVersion) error {
	// 替换成功后不回滚：当前进程仍持有旧 inode，正在处理的请求不受影响；真有
	// 问题也能从 .old 手工换回来。
	if _, _, err := replaceBinaryWithRollback(stagePath, m.executable); err != nil {
		return fmt.Errorf("替换程序文件: %w", err)
	}
	if err := writeReplacedMarker(filepath.Dir(m.executable), version.text); err != nil {
		// 标记写不上不影响本次更新，只是下次启动不会自动清 .old。
		log.Printf("WARN: [updater] 写更新标记失败（不影响更新）: %v", err)
	}
	m.mu.Lock()
	m.pendingReexec = true
	m.status.LastError = ""
	m.status.LastErrorKind = ""
	m.mu.Unlock()
	log.Printf("[updater] 已原地替换 %s → v%s（容器内更新，旧文件保留为 %s.old）",
		m.executable, version.text, filepath.Base(m.executable))
	return nil
}

func writeReplacedMarker(dir, version string) error {
	if dir == "" {
		return errors.New("程序目录为空")
	}
	return os.WriteFile(filepath.Join(dir, replacedMarkerName), []byte(version+"\n"), 0o644)
}

// armInPlaceFinalize 启动时检查容器内更新的标记：版本对得上说明上一个进程被成功
// 换掉了，活过观察窗口（与 helper 路径同一个 childGrace）再删 .old 与标记；
// 版本对不上说明当时 exec 失败、旧版本还在跑，.old 留着。
func (m *Manager) armInPlaceFinalize() {
	if m.executable == "" {
		return
	}
	dir := filepath.Dir(m.executable)
	marker := filepath.Join(dir, replacedMarkerName)
	data, err := os.ReadFile(marker)
	if err != nil {
		return
	}
	installed := strings.TrimSpace(string(data))
	if installed == "" || installed != m.version {
		return
	}
	window := childGrace()
	log.Printf("[updater] 检测到容器内更新已生效（v%s），%s 后清理旧版本备份", installed, window)
	time.AfterFunc(window, func() {
		backup := m.executable + ".old"
		if err := removeFileRetrying(backup, exeReleaseBudget); err != nil {
			log.Printf("WARN: [updater] 清理回滚备份 %s 失败: %v", filepath.Base(backup), err)
		}
		_ = os.Remove(marker)
	})
}

// execSelf 用新二进制接管当前进程。exec 成功时本函数不返回（进程映像已被替换）。
// 失败则复位状态并留下原因：文件已经换好，重启容器就能用上新版本。
func (m *Manager) execSelf() {
	m.mu.Lock()
	if !m.pendingReexec {
		m.mu.Unlock()
		return
	}
	m.pendingReexec = false
	reexec := m.reexec
	m.mu.Unlock()
	if reexec == nil {
		reexec = reexecSelf
	}
	exe := m.executable
	// 原样沿用 argv（含 -config 等启动参数）；argv[0] 不参与执行路径解析。
	argv := append([]string(nil), os.Args...)
	log.Printf("[updater] 容器内更新：用新二进制接管当前进程 pid=%d exe=%s", os.Getpid(), exe)
	if err := reexec(exe, argv); err != nil {
		m.mu.Lock()
		m.status.Installing = false
		m.status.LastError = fmt.Sprintf("替换完成但启动新版本失败: %v（新二进制已就位，重启容器后生效）", err)
		m.mu.Unlock()
		log.Printf("WARN: [updater] 进程内 exec 失败: %v", err)
	}
}
