package userstats

import (
	"bufio"
	"encoding/base64"
	"os"
	"strings"

	E "github.com/sagernet/sing/common/exceptions"
)

// auditFile 是一个身份的审计文件写入状态。
//
// 纪律 3（写失败复位）、4（一条一次写出）、5（启动补残行）、9（inode 检测）都是**逐文件**
// 成立的，因此这些状态必须挂在这里，而不是挂在 writer 上。
type auditFile struct {
	identity string
	path     string
	file     *os.File
	writer   *bufio.Writer
	size     int64
	seq      uint64 // 只由 writer goroutine 访问，不需要原子
}

const (
	auditFilePrefix = "access-"
	auditFileSuffix = ".jsonl"
)

// auditFileName 把计费身份名映射成文件名。
//
// **不能直接用身份名当文件名**：README §3 只要求「每字节为 ASCII 可显示非空白字符」，
// 于是 `../../tmp/x` 是一个完全合法的计费身份名，直接拼进路径就是路径穿越。
// 收紧 §3 会波及计费身份本身（那是结算的键），代价太大，所以映射放在这里。
//
// 用 base64url（RFC 4648 §5）而不是标准 base64：标准表里有 `/`，正好是路径分隔符。
// base64url 的字母表是 A-Za-z0-9-_，既穿越不了也拼不出 `..`；而且它是双射，
// 文件名可以直接解回身份名，按人交付与按人删除都不需要额外的映射表。
func auditFileName(identity string) string {
	return auditFilePrefix + base64.RawURLEncoding.EncodeToString([]byte(identity)) + auditFileSuffix
}

// auditIdentityFromFileName 从文件名反解身份名，供总量封顶时归因用。
//
// 已轮转文件形如 access-<b64>.jsonl.<时间戳>，因此先剥前缀再按第一个 ".jsonl" 截断。
func auditIdentityFromFileName(name string) (string, bool) {
	if !strings.HasPrefix(name, auditFilePrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(name, auditFilePrefix)
	index := strings.Index(rest, auditFileSuffix)
	if index < 0 {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rest[:index])
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

// checkAuditNameCollisions 在启动期检查文件名是否会因大小写折叠而相撞。
//
// **这条主要是开发环境的护栏，不是生产的。** 生产用 ext4，区分大小写，编码后不可能相撞；
// 但开发与测试跑在 macOS 上，APFS 默认不区分大小写，两个身份编出只差大小写的文件名时
// 会被合并进同一个文件——两个人的记录混进一份，正是「数据主体分离」最不能出的错，
// 而且它会表现为「同一份用例在两个平台上结论不同」，那类失败极难归因。
// 启动时跑一次、十几行，留着比省掉划算。
func checkAuditNameCollisions(identities []string) error {
	folded := make(map[string]string, len(identities))
	for _, identity := range identities {
		key := strings.ToLower(auditFileName(identity))
		if previous, exists := folded[key]; exists && previous != identity {
			return E.New("计费身份 ", previous, " 与 ", identity,
				" 的审计文件名在忽略大小写时相同：在不区分大小写的文件系统上会把两个人的记录写进同一个文件")
		}
		folded[key] = identity
	}
	return nil
}

// openAuditFile 打开（或创建）一个审计文件并做启动期硬校验。
//
// 以最终 euid 实际 OpenFile(0600) 一次并 Chmod + Stat 复核属主与权限，任一不符即失败
// （纪律 12 的前半）。随后按纪律 5 补齐崩溃留下的残行。
func openAuditFile(path string, identity string, bufferBytes int) (*auditFile, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, E.Cause(err, "打开访问审计文件 ", path)
	}
	if err = file.Chmod(0o600); err != nil {
		file.Close()
		return nil, E.Cause(err, "设置访问审计文件权限")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, E.Cause(err, "读取访问审计文件属性")
	}
	if info.Mode().Perm() != 0o600 {
		file.Close()
		return nil, E.New("访问审计文件权限不是 0600：", info.Mode().Perm().String())
	}
	if err = checkFileOwner(info); err != nil {
		file.Close()
		return nil, err
	}
	if err = repairTrailingNewline(file, info.Size()); err != nil {
		file.Close()
		return nil, err
	}
	if info, err = file.Stat(); err != nil {
		file.Close()
		return nil, E.Cause(err, "复核访问审计文件大小")
	}
	return &auditFile{
		identity: identity,
		path:     path,
		file:     file,
		writer:   bufio.NewWriterSize(file, bufferBytes),
		size:     info.Size(),
	}, nil
}
