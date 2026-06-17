package service

import (
	"fmt"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
)

// 字段长度上限(对齐 09 DDL 列宽,R2-M3:超长应 400 而非落库溢出 500)。
const (
	maxNameLen  = 128 // name / display_name VARCHAR(128)
	maxSlugLen  = 64  // slug VARCHAR(64)
	maxEmailLen = 191 // login_email VARCHAR(191)
	maxNoteLen  = 512 // note / reason VARCHAR(512)
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

// firstErr 返回第一个非 nil 错误。
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
