package service

// 密钥卫生（SEC 组）的机器判据。
//
// 这一组原本是 human_only：「四类 secret 都是部署时随机生成，且不出现在仓库里」
// 靠人去看。人看会漏两件事，而这两件事恰好是开源白盒项目最致命的：
//
//  1. 仓库里留着一个**可用的**默认凭据。攻击者读得到默认值，于是「用了默认值」
//     与「根本没设口令」在他眼里是同一件事。它不会让任何功能测试变红 —— 恰恰
//     相反，有默认值时本地一跑就通，没人会去改。
//
//  2. 真凭据被顺手粘进仓库（.env / 配置样例 / 调试脚本 / 测试夹具）。一次 commit
//     之后它就永久留在 git 历史里，删掉当前文件不等于撤销泄漏。
//
// 两条落点都选在「值」上而不是「名字」上：
//
//   - SEC:no-default-secrets 判 viper.SetDefault 的**默认值是不是空串**，
//     不判有没有人写文档说「请修改默认口令」。
//   - SEC:secrets-not-in-repo 判**有没有一个看起来像真值的串**，
//     不判有没有出现 "password" 这个词 —— 按名字判会把文档和占位符判红，
//     逼着作者删文档来转绿，那是把门用反了。
//
// 本文件只读仓库，不写任何仓库内文件；阳性标定写在 t.TempDir()（仓库之外）。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 判据落点：配置默认值的唯一真源。SetDefault 是「部署方什么都不填时会拿到什么」
// 的定义处，所以这里是唯一能机器判「有没有可用默认凭据」的地方。
const mirasimSecretsConfigPath = "../config/config.go"

// 仓库工作树根（backend/internal/service → 仓库根）。
const mirasimSecretsRepoRoot = "../../.."

// ---------------------------------------------------------------------------
// SEC:no-default-secrets —— viper.SetDefault 扫描器
// ---------------------------------------------------------------------------

// mirasimDefaultDecl 是一条 viper.SetDefault 调用。
// 刻意不保存「值本身」以外的任何解释：判决只看 IsString + Value 是否为空。
type mirasimDefaultDecl struct {
	Key      string // 字面量 key；动态 key（provider+".client_secret"）保留其字面量片段
	Dynamic  bool
	IsString bool   // 默认值是不是字符串字面量
	Value    string // 字符串默认值（只用于判空，任何失败消息都不回显它）
	Pos      string // file:line，失败消息里只报这个
}

// mirasimScanViperDefaults 用 AST 而不是正则去找 viper.SetDefault。
//
// 为什么必须是 AST：正则会漏掉跨行写法（config.go 里确实有两处 slice 默认值跨行），
// 而漏掉的那条正是最容易藏东西的地方 —— 一个门如果「看不见某种写法」，
// 绕过它的成本就是换个写法，等于没有门。
func mirasimScanViperDefaults(t *testing.T, filename, src string) []mirasimDefaultDecl {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	require.NoError(t, err, "解析 %s 失败；解析不了就得不出任何结论，不能当成通过", filename)

	base := filepath.Base(filename)
	var out []mirasimDefaultDecl
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetDefault" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "viper" || len(call.Args) != 2 {
			return true
		}
		key, dynamic := mirasimDefaultKeyOf(call.Args[0])
		decl := mirasimDefaultDecl{
			Key:     key,
			Dynamic: dynamic,
			Pos:     fmt.Sprintf("%s:%d", base, fset.Position(call.Pos()).Line),
		}
		if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil {
				decl.IsString = true
				decl.Value = v
			}
		}
		out = append(out, decl)
		return true
	})
	return out
}

// mirasimDefaultKeyOf 取出 key。动态 key（provider+".client_secret"）保留字面量
// 片段并标记 Dynamic —— 循环生成的那一批 provider 配置正是「一处改动影响十几个
// 凭据键」的地方，扫描器必须看得见它，否则它会成为唯一的盲区。
func mirasimDefaultKeyOf(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			if v, err := strconv.Unquote(e.Value); err == nil {
				return v, false
			}
		}
	case *ast.BinaryExpr:
		left, _ := mirasimDefaultKeyOf(e.X)
		right, _ := mirasimDefaultKeyOf(e.Y)
		return left + right, true
	}
	return "", true
}

// mirasimCredentialKeyWords 是宽筛：key 里出现这些词就进入候选集。
// 宽筛刻意宁滥勿缺 —— 收窄发生在下面那张**逐条列举**的分类表里，
// 而不是发生在这里。理由：在宽筛里收窄是隐形的（谁也看不出少筛了什么），
// 在分类表里收窄是显式的（每一条都要写出它为什么不是凭据）。
var mirasimCredentialKeyWords = []string{"secret", "password", "passwd", "token", "key", "credential"}

// mirasimNonCredentialDefaults 是命中宽筛、但**可以证明不是凭据**的 key。
//
// 这张表是本门唯一的放宽口子，所以它必须是「精确 key + 理由」，不能是模式匹配：
// 写成 `*_url` 这类模式，新增一个 `admin_token_url` 就会被静默放行；
// 写成精确 key，新增的任何凭据键都会自动落进 offender 名单，必须有人显式分类。
var mirasimNonCredentialDefaults = map[string]string{
	"linuxdo_connect.token_url":          "OAuth token 端点 URL，公开值，不是凭据",
	"dingtalk_connect.token_url":         "OAuth token 端点 URL，公开值，不是凭据",
	"linuxdo_connect.token_auth_method":  "枚举值（client_secret_post），不是凭据本身",
	"oidc_connect.token_auth_method":     "枚举值（client_secret_post），不是凭据本身",
	"batch_image.queue_ready_key":        "Redis 键名（命名空间），不是凭据",
	"batch_image.queue_delayed_key":      "Redis 键名（命名空间），不是凭据",
	"batch_image.queue_active_key":       "Redis 键名（命名空间），不是凭据",
	"batch_image.inflight_key_prefix":    "Redis 键名前缀，不是凭据",
	"batch_image.lock_key_prefix":        "Redis 键名前缀，不是凭据",
	"batch_image.idempotency_key_prefix": "Redis 键名前缀，不是凭据",
	"default.api_key_prefix":             "公开的 API key 前缀 sk-，是格式不是秘密",
	"dashboard_cache.key_prefix":         "Redis 命名空间前缀，不是凭据",
}

// mirasimUsableSecretDefaults 返回「提供了可用凭据默认值」的那些声明。
//
// 判据三条，缺一不可：
//   - key 命中宽筛（可能是凭据）
//   - 默认值是**字符串字面量**（int/bool 默认值当不了口令，放进来只会制造噪声，
//     噪声大的门会被关掉）
//   - 值非空，且不在逐条分类表里
func mirasimUsableSecretDefaults(decls []mirasimDefaultDecl) []mirasimDefaultDecl {
	var out []mirasimDefaultDecl
	for _, d := range decls {
		if !mirasimKeyLooksLikeCredential(d.Key) {
			continue
		}
		if !d.IsString || d.Value == "" {
			continue
		}
		if _, classified := mirasimNonCredentialDefaults[d.Key]; classified {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func mirasimKeyLooksLikeCredential(key string) bool {
	lower := strings.ToLower(key)
	for _, word := range mirasimCredentialKeyWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

func mirasimDefaultKeys(decls []mirasimDefaultDecl) []string {
	keys := make([]string, 0, len(decls))
	for _, d := range decls {
		keys = append(keys, d.Key)
	}
	return keys
}

// mirasimDefaultLocations 是失败消息用的呈现：只有 key 与位置，**没有值**。
// 一条报出明文默认口令的失败消息，会把 CI 日志变成新的泄漏面。
func mirasimDefaultLocations(decls []mirasimDefaultDecl) []string {
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, fmt.Sprintf("%s (%s)", d.Key, d.Pos))
	}
	return out
}

// mirasimSecretsCalibrationSrc 是扫描器的标定夹具：同一份源码，只改 jwt.secret
// 一个开关（有值 / 空串），结论必须反转。
const mirasimSecretsCalibrationSrc = `package config

import "github.com/spf13/viper"

func setDefaults() {
	viper.SetDefault("jwt.secret", %q)
	viper.SetDefault("server.port", 8080)
	viper.SetDefault("linuxdo_connect.token_url", "https://connect.linux.do/oauth2/token")
	viper.SetDefault(provider+".client_secret", "")
}
`

// TestMirasimSecretsNoUsableDefaultCredentials 判「仓库里有没有可用的默认凭据」。
//
// 门开在 internal/config/config.go 的 viper.SetDefault 上：那是「部署方什么都不填
// 时拿到什么」的定义处。判据是**默认值必须是空串**，即强制部署时显式配置 ——
// 空串会在启动校验或连接时立刻炸，而一个能用的默认值会静默地跑起来，
// 这正是「用了默认口令」能活到生产的原因。
//
// 判据若放宽成「只要文档提醒改口令就算过」，漏掉的就是这整类失效：
// 文档提醒不会阻止任何一次部署使用默认值。
func TestMirasimSecretsNoUsableDefaultCredentials(t *testing.T) {
	// [[cov:SEC:no-default-secrets]]

	// ── 阳性标定（差分阴性配对）：先证明扫描器不是空转 ──────────────────
	// 一个永远返回空 offender 的扫描器让下面整条门恒绿。这里喂同一份源码的
	// 两个版本，只改 jwt.secret 的默认值一个开关，要求结论反转。
	armed := mirasimScanViperDefaults(t, "calibration.go",
		fmt.Sprintf(mirasimSecretsCalibrationSrc, "hunter2"))
	require.Equal(t, []string{"jwt.secret"}, mirasimDefaultKeys(mirasimUsableSecretDefaults(armed)),
		"扫描器没抓到被塞进去的非空 jwt.secret 默认值 —— 它对真实文件的结论也就没有意义")

	disarmed := mirasimScanViperDefaults(t, "calibration.go",
		fmt.Sprintf(mirasimSecretsCalibrationSrc, ""))
	require.Len(t, mirasimUsableSecretDefaults(disarmed), 0,
		"只把 jwt.secret 改成空串，结论必须反转；不反转说明判的不是「值」而是别的东西")

	// 分类表确实在起作用：token_url 命中宽筛（含 token）但被逐条分类为非凭据，
	// 所以它不在 armed 的 offender 里。缺了这一条，宽筛会把端点 URL 也判红，
	// 门因为太吵而被关掉，等于没有门。
	require.Contains(t, mirasimNonCredentialDefaults, "linuxdo_connect.token_url",
		"OAuth 端点 URL 必须被显式分类为非凭据，否则本门会淹没在误报里")
	require.Len(t, mirasimNonCredentialDefaults, 12,
		"放宽判据必须是一次显式的、能被 review 的改动：这张表增删一条就要动这个数字")

	// ── 仪器自检：真实文件真的被读进来了 ──────────────────────────────
	raw, err := os.ReadFile(mirasimSecretsConfigPath)
	require.NoError(t, err, "读不到 %s；读不到就得不出结论，不能当成通过", mirasimSecretsConfigPath)
	decls := mirasimScanViperDefaults(t, mirasimSecretsConfigPath, string(raw))
	require.Greater(t, len(decls), 400,
		"只扫到 %d 条 viper.SetDefault —— config.go 有 500 条上下，说明扫描器瞎了", len(decls))

	byKey := make(map[string]mirasimDefaultDecl, len(decls))
	var dynamic, static []mirasimDefaultDecl
	for _, d := range decls {
		if d.Dynamic {
			dynamic = append(dynamic, d)
			continue
		}
		static = append(static, d)
		byKey[d.Key] = d
	}
	// 断言打在 key 列表上而不是 map 上：testify 失败时会把整个对象打印出来，
	// 对 map 断言等于在 CI 日志里回显所有默认值 —— 一条会泄漏的门禁本身就是缺陷。
	staticKeys := mirasimDefaultKeys(static)
	require.Contains(t, staticKeys, "jwt.secret", "扫描器没看见 jwt.secret，后面所有断言都是空转")
	require.Contains(t, staticKeys, "database.password", "扫描器没看见 database.password，后面所有断言都是空转")
	require.Contains(t, mirasimDefaultKeys(dynamic), ".client_secret",
		"循环里生成的 provider.client_secret 必须被看见 —— 一处改动影响十几个凭据键，它是最大的盲区候选")

	// ── 与生产符号绑定：这个 key 真的能落到一个可用的字段上 ─────────────
	// 只断言字符串 key 会漏掉「key 拼错了所以其实没人读」的情况；
	// 反过来，绑定到 config.DatabaseConfig.Password 证明这个默认值是活的。
	dbField, found := reflect.TypeOf(config.Config{}).FieldByName("Database")
	require.Equal(t, true, found, "config.Config 没有 Database 字段，本条的 key 映射假设已经失效")
	require.Equal(t, "database", dbField.Tag.Get("mapstructure"),
		"config.Config.Database 的 mapstructure 名变了，database.* 这一支的判据要跟着改")
	pwField, found := reflect.TypeOf(config.DatabaseConfig{}).FieldByName("Password")
	require.Equal(t, true, found, "config.DatabaseConfig 没有 Password 字段，本条的 key 映射假设已经失效")
	require.Equal(t, "password", pwField.Tag.Get("mapstructure"),
		"database.password 必须真的映射到 config.DatabaseConfig.Password —— 否则判的是一个没人读的 key")

	// ── 逐条钉住运营者点名的那几类凭据 ────────────────────────────────
	for _, key := range []string{
		"jwt.secret",
		"default.admin_password",
		"totp.encryption_key",
		"redis.password",
		"image_storage.secret_access_key",
		"linuxdo_connect.client_secret",
		"oidc_connect.client_secret",
		"gemini.oauth.client_secret",
		"dingtalk_connect.client_secret",
		"wechat_connect.app_secret",
	} {
		require.Contains(t, staticKeys, key, "%s 不在 config.go 里了；判据要跟着真源走", key)
		// 比长度而不是比值：失败时也绝不能把默认口令打进 CI 日志。
		require.Equal(t, 0, len(byKey[key].Value),
			"%s 提供了长度 %d 的默认值（位置 %s）；必须是空串 = 强制部署时显式配置",
			key, len(byKey[key].Value), byKey[key].Pos)
	}

	// ── 全量门：任何「像凭据的 key 带着非空字符串默认值」都要被点名 ──────
	offenders := mirasimUsableSecretDefaults(decls)
	require.Len(t, mirasimDefaultLocations(offenders), 0,
		"仓库为凭据类配置提供了可用的默认值（只报 key 与位置，不回显值）：%v；"+
			"sub2api 是开源白盒，默认值全世界都读得到，「用了默认值」与「没有口令」等价",
		mirasimDefaultLocations(offenders))
}

// ---------------------------------------------------------------------------
// SEC:secrets-not-in-repo —— 工作树凭据扫描
// ---------------------------------------------------------------------------

// mirasimSecretShape 是「看起来像真凭据」的形态。
// 注意每条正则都判**值**：凭据名后面必须跟着一个足够长、非占位符的串。
// 按名字判（出现 "token" 就红）会命中文档与占位符，逼作者删文档转绿。
type mirasimSecretShape struct {
	Name string
	Re   *regexp.Regexp
}

// 模式串用拼接写法，保证本文件自身的源码不会命中自己（除此之外本文件还在豁免表里，
// 两道保险：光靠豁免表，一旦文件被改名就会自命中并把门变成噪声）。
var mirasimSecretShapes = []mirasimSecretShape{
	{"api-key-sk-hex64", regexp.MustCompile(`\b` + "sk" + `-[0-9a-f]{64}\b`)},
	{"jwt-secret-hex64", regexp.MustCompile(`(?i)jwt[_.\-]?secret["']?\s*[:=]\s*["']?[0-9a-f]{64}`)},
	{"secret-hex64-nested", regexp.MustCompile(`(?i)^\s*secret\s*[:=]\s*["']?[0-9a-f]{64}`)},
	{"resin-token", regexp.MustCompile(`RESIN_(?:ADMIN|PROXY)_TOKEN\s*[:=]\s*["']?[A-Za-z0-9+/=_-]{16,}`)},
}

// mirasimSecretScanSkipDirs：目录整棵跳过。二进制/依赖树里搜不出有意义的结论，
// 却能把一次扫描拖到分钟级 —— 慢到没人跑的门等于没有门。
var mirasimSecretScanSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, ".next": true, "coverage": true, ".venv": true,
}

// mirasimSecretScanWaivers：命中但**不是真凭据**的文件，精确到路径 + 理由。
//
// 只豁免文件、不豁免整类（例如「所有 _test.go」）：测试文件正是最常被粘进真 token
// 的地方，整类豁免会把最大的泄漏面挖成盲区。代价写在这里：被豁免的这几个文件里
// 如果有人粘进真凭据，本门看不见 —— 这是精确豁免能换到的最小代价。
var mirasimSecretScanWaivers = map[string]string{
	"backend/internal/service/mirasim_secrets_hygiene_test.go":     "本门自身的模式串与标定样本",
	"backend/internal/service/mirasim_deploy_proxy_test.go":        "同批 DP 门的假代理口令夹具",
	"backend/internal/service/mirasim_deploy_resin_config_test.go": "另一条 SEC 门的阳性标定样本（合成值，非真凭据）",
}

// mirasimPlaceholderMarkers：占位符（$VAR / ${VAR} / <your-token> / ***）不是凭据。
var mirasimPlaceholderMarkers = []string{"$", "{", "<", "*", "xxxx", "your-", "changeme", "example"}

// mirasimScanTreeForSecrets 扫描一棵目录树，返回命中位置（**不含值**）与实际读过的
// 文件数。文件数是仪器自检用的：一次「0 命中」必须能和「0 文件」区分开。
func mirasimScanTreeForSecrets(t *testing.T, root string, waived map[string]string) ([]string, int) {
	t.Helper()
	var hits []string
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不到的条目跳过，不把整次扫描搞崩
		}
		if d.IsDir() {
			if mirasimSecretScanSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if _, ok := waived[rel]; ok {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || info.Size() > 2<<20 {
			return nil // 超过 2MB 的多半是数据/产物，不是人手写的凭据落点
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		head := data
		if len(head) > 8192 {
			head = head[:8192]
		}
		if strings.IndexByte(string(head), 0) >= 0 || !utf8.Valid(data) {
			return nil // 二进制
		}
		scanned++
		for lineNo, line := range strings.Split(string(data), "\n") {
			for _, shape := range mirasimSecretShapes {
				match := shape.Re.FindString(line)
				if match == "" || mirasimLooksLikePlaceholder(match) {
					continue
				}
				// 只报位置与形态名，绝不回显命中的串本身。
				hits = append(hits, fmt.Sprintf("%s:%d [%s]", rel, lineNo+1, shape.Name))
			}
		}
		return nil
	})
	require.NoError(t, err, "遍历 %s 失败；遍历不了就得不出结论，不能当成通过", root)
	sort.Strings(hits)
	return hits, scanned
}

func mirasimLooksLikePlaceholder(match string) bool {
	lower := strings.ToLower(match)
	for _, marker := range mirasimPlaceholderMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// TestMirasimSecretsAreNotCommittedToTheRepo 判「真凭据有没有被粘进工作树」。
//
// 覆盖三种在本项目真实存在的形态：上游 API key（sk- + 64 位十六进制）、
// jwt secret 赋值（64 位十六进制）、resin 的 RESIN_ADMIN/PROXY_TOKEN 赋值。
//
// 判据若放宽成「按凭据名判」，会把 deploy/apply-resin-runtime.sh 里
// `${RESIN_ADMIN_TOKEN}` 这种正确写法判红 —— 那条脚本恰恰是「token 只从
// /etc/resin.env 取、绝不落仓库」的实现，把它判红等于惩罚正确做法。
func TestMirasimSecretsAreNotCommittedToTheRepo(t *testing.T) {
	// [[cov:SEC:secrets-not-in-repo]]

	// ── 形态与生产符号对齐 ────────────────────────────────────────────
	// 扫描器认的是「值的形状」，但形状是从生产配置键推出来的：
	// jwt secret 的落点是 config.JWTConfig.Secret（mapstructure "secret"），
	// sk- 前缀来自 config.DefaultConfig.APIKeyPrefix。这两个键一旦改名，
	// 下面的正则就会对着一个不存在的形态扫，必须在这里当场红掉，
	// 而不是安静地扫出 0 命中 —— 那才是最危险的绿。
	jwtField, found := reflect.TypeOf(config.JWTConfig{}).FieldByName("Secret")
	require.Equal(t, true, found, "config.JWTConfig 没有 Secret 字段，jwt-secret 形态的假设已失效")
	require.Equal(t, "secret", jwtField.Tag.Get("mapstructure"),
		"jwt secret 的配置键名变了，jwt-secret-hex64 / secret-hex64-nested 两个形态要跟着改")
	prefixField, found := reflect.TypeOf(config.DefaultConfig{}).FieldByName("APIKeyPrefix")
	require.Equal(t, true, found, "config.DefaultConfig 没有 APIKeyPrefix 字段，sk- 形态的假设已失效")
	require.Equal(t, "api_key_prefix", prefixField.Tag.Get("mapstructure"),
		"API key 前缀的配置键名变了，api-key-sk-hex64 形态要跟着改")
	require.Len(t, mirasimSecretShapes, 4,
		"四种形态（sk- key / jwt secret 赋值 / 嵌套 secret / resin token）少一种，对应那类泄漏就恒绿")

	// ── 阳性标定：把三种假凭据种进临时目录（仓库之外，不污染工作树） ──────
	// 假值在运行时拼出来，源码里不出现 64 位十六进制字面量，
	// 否则本文件会被自己的扫描器命中，门就退化成「谁也不许写测试」。
	fakeHex := strings.Repeat("ab", 32)
	planted := strings.Join([]string{
		"ANTHROPIC_KEY=" + "sk" + "-" + fakeHex,
		"JWT_SECRET=" + fakeHex,
		"RESIN_ADMIN_TOKEN=" + strings.Repeat("Z", 24),
	}, "\n")
	armedDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(armedDir, "planted.env"), []byte(planted), 0o600))

	armedHits, armedScanned := mirasimScanTreeForSecrets(t, armedDir, nil)
	require.Equal(t, 1, armedScanned, "标定目录里就一个文件，扫描器却读了 %d 个", armedScanned)
	require.Len(t, armedHits, 3,
		"三种凭据形态必须逐一被抓到（实际命中：%v）；漏掉任何一种，真实仓库的那次扫描对它恒绿", armedHits)
	require.Contains(t, armedHits[0], "planted.env:1 [api-key-sk-hex64]", "sk- 形态没被抓到")

	// ── 差分阴性：同一份内容，只把「值」换成占位符，结论必须反转 ───────────
	// 没有这一半，把判据放宽成「出现 RESIN_ADMIN_TOKEN 就算」也能让阳性全过，
	// 代价是所有文档与 shell 变量引用全被判红。
	defused := strings.Join([]string{
		"ANTHROPIC_KEY=" + "sk" + "-${ANTHROPIC_KEY}",
		"JWT_SECRET=${JWT_SECRET}",
		"RESIN_ADMIN_TOKEN=${RESIN_ADMIN_TOKEN}",
	}, "\n")
	defusedDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(defusedDir, "planted.env"), []byte(defused), 0o600))
	defusedHits, _ := mirasimScanTreeForSecrets(t, defusedDir, nil)
	require.Len(t, defusedHits, 0,
		"占位符被判成凭据（%v）—— 这样的门会逼着作者删掉正确的变量引用来转绿", defusedHits)

	// ── 真实工作树 ────────────────────────────────────────────────────
	repoHits, repoScanned := mirasimScanTreeForSecrets(t, mirasimSecretsRepoRoot, mirasimSecretScanWaivers)
	require.Greater(t, repoScanned, 1000,
		"只扫到 %d 个文本文件 —— 仓库里有数千个，说明遍历被截断了，这次「0 命中」不算数", repoScanned)
	require.Len(t, repoHits, 0,
		"工作树里发现疑似凭据（只报位置，不回显值）：%v；一次 commit 之后它就永久留在 git 历史里", repoHits)
}
