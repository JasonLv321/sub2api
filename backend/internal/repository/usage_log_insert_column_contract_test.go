//go:build unit

package repository

import (
	"database/sql"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// usage_log_repo_insert.go 靠位置绑定 60 个参数，没有任何编译期保护：
// 列清单、usageLogInsertArgTypes、prepareUsageLogInsert().args 三者一旦错位，
// 计费列会被静默写进相邻列。本测试把那条只写在注释里的契约变成可执行断言。
func TestUsageLogInsertColumnListsMatchArgTypes(t *testing.T) {
	src := readRepoSource(t, "usage_log_repo_insert.go")

	// 每个 INSERT INTO usage_logs ( ... ) 的列清单都必须与参数类型表等长。
	re := regexp.MustCompile(`(?s)INSERT INTO usage_logs \(\s*(.*?)\)`)
	matches := re.FindAllStringSubmatch(src, -1)
	require.NotEmpty(t, matches, "没找到任何 INSERT INTO usage_logs，测试本身失效了")

	for i, m := range matches {
		cols := splitColumnList(m[1])
		require.Equal(t, len(usageLogInsertArgTypes), len(cols),
			"第 %d 个 INSERT 的列数与 usageLogInsertArgTypes 不一致：%v", i+1, cols)
		require.Equal(t, "created_at", cols[len(cols)-1],
			"第 %d 个 INSERT 的最后一列应是 created_at", i+1)
	}
}

// 显式占位符清单（$1..$N）必须覆盖全部列，少一个就会整体错位。
func TestUsageLogInsertPlaceholderCountMatchesColumns(t *testing.T) {
	src := readRepoSource(t, "usage_log_repo_insert.go")
	re := regexp.MustCompile(`\) VALUES \(\s*((?:\s*\$\d+,?)+)\s*\)`)
	matches := re.FindAllStringSubmatch(src, -1)
	require.NotEmpty(t, matches, "没找到显式占位符清单，测试本身失效了")

	for i, m := range matches {
		n := len(regexp.MustCompile(`\$\d+`).FindAllString(m[1], -1))
		require.Equal(t, len(usageLogInsertArgTypes), n,
			"第 %d 个占位符清单有 %d 个 $N，应为 %d", i+1, n, len(usageLogInsertArgTypes))
	}
}

// 读回路径的列清单同样必须逐列对齐，否则 scanUsageLog 会读错列。
func TestUsageLogSelectColumnsMatchInsertColumns(t *testing.T) {
	cols := splitColumnList(usageLogSelectColumns)
	// select 多一个 id（自增主键，不在插入清单里）。
	require.Equal(t, "id", cols[0])
	require.Equal(t, len(usageLogInsertArgTypes)+1, len(cols),
		"usageLogSelectColumns 与插入列清单不等长：%v", cols)
}

func splitColumnList(raw string) []string {
	out := make([]string, 0, 64)
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readRepoSource 读取本包内的源文件。契约存在于 SQL 字符串里，没有别的办法核。
func readRepoSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	require.NoError(t, err, "读不到 %s", name)
	return string(b)
}

// request_payload_hash 是倒数第二个参数（created_at 永远最后）。与 session_id 的
// 同类测试对称，钉住新列的位置，避免以后再加列时把它挤错位。
func TestPrepareUsageLogInsert_RequestPayloadHashArgWiring(t *testing.T) {
	hash := strings.Repeat("a", 64)
	log := &service.UsageLog{
		UserID: 1, APIKeyID: 2, AccountID: 3,
		RequestID: "req-payload-hash", Model: "claude-3",
		InputTokens: 10, OutputTokens: 5, TotalCost: 1.0, ActualCost: 1.0,
		RequestPayloadHash: &hash,
		CreatedAt:          time.Now().UTC(),
	}
	prepared := prepareUsageLogInsert(log)
	require.Len(t, prepared.args, len(usageLogInsertArgTypes))

	arg := prepared.args[len(prepared.args)-2]
	ns, ok := arg.(sql.NullString)
	require.True(t, ok, "request_payload_hash 应是 sql.NullString，实际 %T", arg)
	require.True(t, ns.Valid)
	require.Equal(t, hash, ns.String)
	require.Equal(t, "text", usageLogInsertArgTypes[len(usageLogInsertArgTypes)-2])

	// 缺失时必须是 SQL NULL，不能写成空串 —— 否则"没采到请求体"和"请求体为空"分不开。
	log.RequestPayloadHash = nil
	arg = prepareUsageLogInsert(log).args[len(prepared.args)-2]
	ns, ok = arg.(sql.NullString)
	require.True(t, ok)
	require.False(t, ns.Valid)
}
