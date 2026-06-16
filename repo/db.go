// Package repo 是平台自有库(MySQL)的读写收口(10 §4.3)。
// 乐观锁在此层落地;绝不碰 new-api 的库/缓存(零数据侵入铁律)。
// 所有业务查询强制带 org_id 谓词(09 §0:默认拒绝跨租户)。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/migrations"
)

// ErrNotFound 表示按条件未查到行(service 据此返 30001/404)。
var ErrNotFound = errors.New("repo: 记录不存在")

// ErrConflict 表示唯一键冲突(重名/邮箱已存在,service 据此返 30003/409)。
var ErrConflict = errors.New("repo: 唯一约束冲突")

// ErrOptimisticLock 表示乐观锁版本不匹配(并发冲突,service 据此返 20904/409)。
var ErrOptimisticLock = errors.New("repo: 乐观锁冲突")

// Store 持有库连接池,聚合各资源 repo。
type Store struct {
	db *sql.DB
}

// Open 用 DSN 打开连接池并探活。DSN 形如:
//
//	user:pass@tcp(host:3306)/dbname?parseTime=true&loc=UTC&charset=utf8mb4
//
// 调用方负责在 DSN 里带 parseTime=true(否则 DATETIME 扫不进 time.Time)。
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("repo: 打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("repo: 数据库探活失败: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭连接池。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接池(集成测试/迁移用)。
func (s *Store) DB() *sql.DB { return s.db }

// Migrate 按文件名顺序执行内嵌迁移(幂等:全 CREATE TABLE IF NOT EXISTS)。
// 逐条语句执行(不依赖 multiStatements),把 SQL 按分号切分并跳过注释/空白。
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("repo: 读取迁移目录失败: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, name := range files {
		raw, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("repo: 读取迁移 %s 失败: %w", name, err)
		}
		for _, stmt := range splitSQL(string(raw)) {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("repo: 执行迁移 %s 失败: %w\n语句: %.120s", name, err, stmt)
			}
		}
	}
	return nil
}

// splitSQL 把迁移文件切成可独立执行的语句:逐行剥掉 -- 行内/整行注释(避免注释里的
// 分号把语句劈开),再按 ; 分割。本项目迁移的字符串字面量不含 "--",故按行截断安全。
func splitSQL(s string) []string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i] // 剥掉行内注释到行尾(连同整行注释)
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	var out []string
	for _, part := range strings.Split(b.String(), ";") {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}

// isDupKey 报告错误是否为 MySQL 唯一键冲突(errno 1062)。
func isDupKey(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1062
	}
	return false
}
