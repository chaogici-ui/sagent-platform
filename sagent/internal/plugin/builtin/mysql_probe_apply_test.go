package builtin

import (
	"testing"

	"github.com/sagent/core/internal/config"
)

// R0-1 托管目标热更语义（无进程路径，进程路径由端到端演练验证）：
//   - 未启动时 ApplyTargets 只更新目标集，不报错；
//   - 目标集未变 → 幂等 no-op；
//   - 任务清零 → 目标集清空。
func TestMySQLProbeApplyTargetsTargetSetSemantics(t *testing.T) {
	m := NewMySQLProbe(config.MySQLProbeConfig{})
	a := config.MySQLTarget{DSN: "root:pass@tcp(mysql-1:3306)/", ResourceID: "db-1"}
	b := config.MySQLTarget{DSN: "root:pass@tcp(mysql-2:3306)/", ResourceID: "db-2"}

	// 未启动：只更新目标集
	changed, err := m.ApplyTargets([]config.MySQLTarget{a})
	if err != nil || !changed {
		t.Fatalf("first apply should change target set: changed=%t err=%v", changed, err)
	}
	if m.Running() {
		t.Fatal("must not be running before StartWith")
	}
	// 同集：幂等 no-op
	changed, err = m.ApplyTargets([]config.MySQLTarget{a})
	if err != nil || changed {
		t.Fatalf("identical target set must be a no-op: changed=%t err=%v", changed, err)
	}
	// 换目标（顺序无关等价判断的反例：不同集合必须生效）
	changed, err = m.ApplyTargets([]config.MySQLTarget{b, a})
	if err != nil || !changed {
		t.Fatalf("different target set must apply: changed=%t err=%v", changed, err)
	}
	// 清零
	changed, err = m.ApplyTargets(nil)
	if err != nil || !changed {
		t.Fatalf("empty target set must apply: changed=%t err=%v", changed, err)
	}
}

func TestMySQLProbeSameTargetsOrderInsensitive(t *testing.T) {
	a := config.MySQLTarget{DSN: "u:p@tcp(h1:3306)/", ResourceID: "db-1"}
	b := config.MySQLTarget{DSN: "u:p@tcp(h2:3306)/", ResourceID: "db-2"}
	if !sameTargets([]config.MySQLTarget{a, b}, []config.MySQLTarget{b, a}) {
		t.Fatal("target set equality must be order-insensitive")
	}
	if sameTargets([]config.MySQLTarget{a}, []config.MySQLTarget{b}) {
		t.Fatal("different targets must not be equal")
	}
}
