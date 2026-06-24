package service

import "strconv"

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// atoi64 解析十进制 int64(解析失败返 0)。用于把看板 byMember 的 string key(new-api user_id)反算回 int64。
func atoi64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
