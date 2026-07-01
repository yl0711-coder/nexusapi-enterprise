package service

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
)

// 格式校验(R2-轻微:slug/admin_email 不校验格式 → 可建无法登录的管理员/坏 slug)。
var (
	// slug:小写字母/数字/连字符,2–64,首尾非连字符(URL/分组名友好)。
	// 首尾各一个非连字符字符 + 中间 0–62 → 长度 2–64,单字符不通过(T4)。
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$`)
	// email:务实校验(非 RFC 全量),挡住明显非法,避免建出登录不了的管理员。
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// checkSlug 校验组织 slug 格式 + 长度。
func checkSlug(slug string) error {
	if !slugRe.MatchString(slug) {
		return apperr.InvalidParam("slug 须为小写字母/数字/连字符,2–64 位且首尾非连字符")
	}
	return nil
}

// checkEmail 校验邮箱格式 + 长度(空串交由各调用方决定是否必填)。
func checkEmail(field, email string) error {
	if len(email) > maxEmailLen {
		return apperr.InvalidParam(fmt.Sprintf("%s 超长(上限 %d 字符)", field, maxEmailLen))
	}
	if !emailRe.MatchString(email) {
		return apperr.InvalidParam(field + " 格式非法")
	}
	return nil
}

// loginNameRe 用户名(非邮箱)登录名:字母/数字/`.`/`_`/`-`,2–64,首尾为字母数字。
var loginNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}[a-zA-Z0-9]$`)

// checkLoginName 校验成员自定义登录名(T11:可填真实邮箱或用户名)。
// 含 `@` → 按邮箱格式校验;否则按用户名校验。空串由调用方决定是否走 fallback。
func checkLoginName(val string) error {
	if len(val) > maxEmailLen {
		return apperr.InvalidParam(fmt.Sprintf("登录名超长(上限 %d 字符)", maxEmailLen))
	}
	if strings.ContainsRune(val, '@') {
		if !emailRe.MatchString(val) {
			return apperr.InvalidParam("登录邮箱格式非法")
		}
		return nil
	}
	if !loginNameRe.MatchString(val) {
		return apperr.InvalidParam("登录名须为字母/数字/.或_或-,2–64 位且首尾为字母数字")
	}
	return nil
}

// 字段长度上限(对齐 09 DDL 列宽,R2-M3:超长应 400 而非落库溢出 500)。
const (
	maxNameLen       = 128 // name / display_name VARCHAR(128)
	maxEmailLen      = 191 // login_email VARCHAR(191)
	maxNoteLen       = 512 // note / reason VARCHAR(512)
	maxTransferNoLen = 128 // transfer_no 入账幂等键,与 DB 列 VARCHAR(128) 一致(T3/T16:此前注释误写 190)
	// maxAdjustQuota 单次调额绝对值上限(动钱面防误填天量,R2-M4)。1e14 quota = 2 亿元。
	maxAdjustQuota int64 = 100_000_000_000_000
)

// checkLen 校验字符串字段长度上限。
func checkLen(field, val string, max int) error {
	if len(val) > max {
		return apperr.InvalidParam(fmt.Sprintf("%s 超长(上限 %d 字符)", field, max))
	}
	return nil
}

// checkName 在长度基础上加 HTML/JS 危险字符二道闸(GZ-05 修复A 后端二道闸),用于 name/display_name
// 类落库字段。即便前端某处遗漏转义,带 < > " ' ` \ 或控制字符的名称也进不了库。
// 用黑名单(非白名单)以免误伤中英文/数字/空格/常见标点等合法名称。
func checkName(field, val string, max int) error {
	if err := checkLen(field, val, max); err != nil {
		return err
	}
	if strings.ContainsAny(val, "<>\"'`\\") {
		return apperr.InvalidParam(field + " 含非法字符(不允许 < > \" ' ` \\)")
	}
	for _, r := range val {
		if r < 0x20 || r == 0x7f {
			return apperr.InvalidParam(field + " 含控制字符")
		}
	}
	return nil
}

// firstErr 返回第一个非 nil 错误。
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
