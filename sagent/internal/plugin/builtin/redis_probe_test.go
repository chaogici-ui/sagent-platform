package builtin

import (
	"testing"

	"github.com/sagent/core/internal/config"
)

// B4b 收口回归：口令是目标集的一部分。
// 曾因 sameRedisTargets 判等漏掉 Password，last-good 恢复（无口令）→ 新 cfg（有口令）
// 被 diff 判「无变化」跳过重启，exporter 永远裸连（NOAUTH 无法自愈）。
func TestSameRedisTargetsPasswordChange(t *testing.T) {
	oldSet := []config.RedisTarget{{Address: "10.20.0.16", Port: "6379", ResourceID: "r1", Password: ""}}
	newSet := []config.RedisTarget{{Address: "10.20.0.16", Port: "6379", ResourceID: "r1", Password: "sagent123"}}
	if sameRedisTargets(oldSet, newSet) {
		t.Fatal("口令变更必须判定为目标集变更（触发 scrape 循环/进程重启）")
	}
	if !sameRedisTargets(oldSet, []config.RedisTarget{{Address: "10.20.0.16", Port: "6379", ResourceID: "r1", Password: ""}}) {
		t.Fatal("同地址同口令应判定无变化（保持进程零抖动复用）")
	}
}

// redis_exporter 多目标模式全实例共用 --redis.password（官方 README）：
// 口令统一 → (pass,true)；口令不统一 → fail-closed 拒绝启动，绝不拿 A 口令静默连 B 实例
func TestRedisUniformPassword(t *testing.T) {
	if p, ok := redisUniformPassword([]config.RedisTarget{
		{Address: "a", Password: "x"}, {Address: "b", Password: "x"},
	}); !ok || p != "x" {
		t.Fatalf("同口令应返回 (x,true)，got (%q,%v)", p, ok)
	}
	if p, ok := redisUniformPassword([]config.RedisTarget{
		{Address: "a", Password: ""}, {Address: "b", Password: ""},
	}); !ok || p != "" {
		t.Fatalf("全空口令应返回 ('',true)，got (%q,%v)", p, ok)
	}
	if _, ok := redisUniformPassword([]config.RedisTarget{
		{Address: "a", Password: "x"}, {Address: "b", Password: "y"},
	}); ok {
		t.Fatal("口令不统一必须判 false（fail-closed）")
	}
}

// target URL 不再内嵌口令（redis_exporter 多目标模式不解析，实测 NOAUTH）
func TestRedisTargetURLNoPassword(t *testing.T) {
	u := redisTargetURL(config.RedisTarget{Address: "10.20.0.16", Port: "6379", Password: "sagent123"})
	if u != "redis://10.20.0.16:6379" {
		t.Fatalf("target URL 不应含口令，got %q", u)
	}
	if u := redisTargetURL(config.RedisTarget{Address: "h", Port: ""}); u != "redis://h:6379" {
		t.Fatalf("端口空应默认 6379，got %q", u)
	}
}
