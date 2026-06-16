// Package migrations 内嵌建库 SQL,供进程启动时幂等建表(MVP 单实例迁移即建)。
// SQL 全用 CREATE TABLE IF NOT EXISTS,重复执行安全。
package migrations

import "embed"

// FS 内嵌本目录下所有 .sql 迁移文件。
//
//go:embed *.sql
var FS embed.FS
