package services

import (
	"errors"
	"testing"
	"time"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// 时区回归测试：测试进程与数据库统一使用 testTimeZone（非 UTC），
// 验证决策有效期在本地时区下不偏移。expires_at 必须为 timestamptz，
// 否则本地时区下过期判断会整体偏移，过期决策仍能被确认。

// dumpTimezoneDiagnostics 过期拒绝失败时输出诊断信息，
// 直接定位问题在时间比较（各时间值/偏移）还是存储类型（列类型）
func dumpTimezoneDiagnostics(t *testing.T, decisionID uint) {
	t.Helper()

	var sessionTZ, columnType, rawExpires string
	var dbNow time.Time
	if err := database.DB.Raw("SHOW TimeZone").Row().Scan(&sessionTZ); err != nil {
		t.Logf("诊断: SHOW TimeZone 失败: %v", err)
	}
	if err := database.DB.Raw("SELECT now()").Row().Scan(&dbNow); err != nil {
		t.Logf("诊断: SELECT now() 失败: %v", err)
	}
	if err := database.DB.Raw(
		`SELECT data_type FROM information_schema.columns
		 WHERE table_name = 'irrigation_decisions' AND column_name = 'expires_at'`).
		Row().Scan(&columnType); err != nil {
		t.Logf("诊断: 查询列类型失败: %v", err)
	}
	if err := database.DB.Raw(
		`SELECT expires_at::text FROM irrigation_decisions WHERE id = ?`, decisionID).
		Row().Scan(&rawExpires); err != nil {
		t.Logf("诊断: 读取 expires_at 失败: %v", err)
	}

	var decision models.IrrigationDecision
	if err := database.DB.First(&decision, decisionID).Error; err != nil {
		t.Logf("诊断: 读取决策失败: %v", err)
	}

	now := time.Now()
	t.Logf("诊断[存储类型]: expires_at 列类型=%q（期望 timestamp with time zone）", columnType)
	t.Logf("诊断[时区]: 进程 time.Local=%v，数据库会话 TimeZone=%q", time.Local, sessionTZ)
	t.Logf("诊断[时间比较]: 进程 now=%s unix=%d", now.Format(time.RFC3339Nano), now.Unix())
	t.Logf("诊断[时间比较]: 数据库 now()=%s unix=%d", dbNow.Format(time.RFC3339Nano), dbNow.Unix())
	t.Logf("诊断[时间比较]: Go ExpiresAt=%s unix=%d location=%v",
		decision.ExpiresAt.Format(time.RFC3339Nano), decision.ExpiresAt.Unix(), decision.ExpiresAt.Location())
	t.Logf("诊断[时间比较]: 库存储文本 expires_at=%q", rawExpires)
	t.Logf("诊断[时间比较]: now.Before(ExpiresAt)=%v，差值=%s（过期决策应为 false/负值）",
		now.Before(decision.ExpiresAt), decision.ExpiresAt.Sub(now))
}

// 前提校验：测试进程与数据库时区一致，且 expires_at 列类型为 timestamptz
func TestTimezoneAlignmentGuard(t *testing.T) {
	if time.Local.String() != testTimeZone {
		t.Fatalf("进程时区=%v，期望 %s", time.Local, testTimeZone)
	}

	var sessionTZ string
	if err := database.DB.Raw("SHOW TimeZone").Row().Scan(&sessionTZ); err != nil {
		t.Fatalf("SHOW TimeZone: %v", err)
	}
	if sessionTZ != testTimeZone {
		t.Fatalf("数据库会话时区=%q，期望 %q（测试进程与数据库必须使用同一时区）", sessionTZ, testTimeZone)
	}

	// 进程与数据库当前时间偏移应远小于时区差（同一物理时刻）
	var dbNow time.Time
	if err := database.DB.Raw("SELECT now()").Row().Scan(&dbNow); err != nil {
		t.Fatalf("SELECT now(): %v", err)
	}
	if skew := time.Since(dbNow); skew < -time.Minute || skew > time.Minute {
		t.Fatalf("进程与数据库时钟偏差过大: %s", skew)
	}

	var columnType string
	if err := database.DB.Raw(
		`SELECT data_type FROM information_schema.columns
		 WHERE table_name = 'irrigation_decisions' AND column_name = 'expires_at'`).
		Row().Scan(&columnType); err != nil {
		t.Fatalf("查询 expires_at 列类型: %v", err)
	}
	if columnType != "timestamp with time zone" {
		t.Fatalf("存储类型错误: expires_at 列类型=%q，期望 timestamp with time zone（timestamp 不带时区会导致过期判断在本地时区偏移）", columnType)
	}
	t.Logf("前提校验通过: 进程时区=%v，数据库时区=%s，expires_at 列类型=%s", time.Local, sessionTZ, columnType)
}

// 有效决策：非 UTC 时区下确认成功，过期时间写入/读取为同一瞬时
func TestTimezoneValidDecision(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "时区玫瑰园")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)

	result := mustEvaluate(t, svc, zone.ID, 600)

	// 过期时间在数据库侧仍然有效（未来）
	var validInDB bool
	if err := database.DB.Raw(
		`SELECT expires_at > now() FROM irrigation_decisions WHERE id = ?`, result.DecisionID).
		Row().Scan(&validInDB); err != nil {
		t.Fatalf("check expires_at in db: %v", err)
	}
	if !validInDB {
		dumpTimezoneDiagnostics(t, result.DecisionID)
		t.Fatal("有效决策在数据库侧被判为过期")
	}

	// 写入与读取是同一瞬时（PostgreSQL 时间戳为微秒精度，按微秒容差比较）
	var stored time.Time
	if err := database.DB.Raw(
		`SELECT expires_at FROM irrigation_decisions WHERE id = ?`, result.DecisionID).
		Row().Scan(&stored); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if skew := stored.Sub(result.ExpiresAt); skew < -time.Microsecond || skew > time.Microsecond {
		dumpTimezoneDiagnostics(t, result.DecisionID)
		t.Fatalf("expires_at 读写非同一瞬时: 写入=%s unix=%d，读回=%s unix=%d，偏差=%s",
			result.ExpiresAt.Format(time.RFC3339Nano), result.ExpiresAt.Unix(),
			stored.Format(time.RFC3339Nano), stored.Unix(), skew)
	}

	// 确认成功
	log, err := svc.ConfirmExecution(result.DecisionID, zone.ID, 600)
	if err != nil {
		dumpTimezoneDiagnostics(t, result.DecisionID)
		t.Fatalf("有效决策确认失败: %v", err)
	}
	if log.Status != models.ExecutionStatusInProgress {
		t.Fatalf("expected status=in_progress, got %s", log.Status)
	}
	t.Logf("有效决策场景: expires_at 读写同一瞬时（unix=%d），确认成功", stored.Unix())
}

// 过期拒绝：数据库侧已过期 1 分钟的决策必须被拒绝；失败时输出时间比较/存储类型诊断
func TestTimezoneExpiredDecision(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "时区温室")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)

	result := mustEvaluate(t, svc, zone.ID, 600)

	// 数据库侧将过期时间改到 1 分钟前（真实的过期瞬时，不经过客户端编码）
	if err := database.DB.Exec(
		`UPDATE irrigation_decisions SET expires_at = now() - interval '1 minute' WHERE id = ?`,
		result.DecisionID).Error; err != nil {
		t.Fatalf("expire decision: %v", err)
	}

	// 前置校验：数据库确认该决策已过期
	var expiredInDB bool
	if err := database.DB.Raw(
		`SELECT expires_at <= now() FROM irrigation_decisions WHERE id = ?`, result.DecisionID).
		Row().Scan(&expiredInDB); err != nil {
		t.Fatalf("check expired in db: %v", err)
	}
	if !expiredInDB {
		dumpTimezoneDiagnostics(t, result.DecisionID)
		t.Fatal("前置校验失败: 数据库侧决策未过期")
	}

	if _, err := svc.ConfirmExecution(result.DecisionID, zone.ID, 300); !errors.Is(err, ErrDecisionExpired) {
		dumpTimezoneDiagnostics(t, result.DecisionID)
		t.Fatalf("过期决策仍能被确认: expected ErrDecisionExpired, got %v", err)
	}
	if n := countLogs(t, zone.ID, ""); n != 0 {
		t.Fatalf("expected no execution log for expired decision, got %d", n)
	}
	t.Log("过期拒绝场景: 本地时区下过期决策被正确拒绝，未产生执行记录")
}

// 区域不一致：非 UTC 时区下确认区域与决策区域不同被拒绝
func TestTimezoneZoneMismatch(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "时区苗圃")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)
	other := mustCreateZone(t, "时区花房")
	mustCreateDevice(t, "valve2", models.DeviceTypeValve, other.ID, models.DeviceStatusOnline)

	result := mustEvaluate(t, svc, zone.ID, 600)

	if _, err := svc.ConfirmExecution(result.DecisionID, other.ID, 300); !errors.Is(err, ErrDecisionZoneMismatch) {
		t.Fatalf("expected ErrDecisionZoneMismatch, got %v", err)
	}
	if n := countLogs(t, other.ID, ""); n != 0 {
		t.Fatalf("expected no execution log on zone mismatch, got %d", n)
	}
	t.Log("区域不一致场景: 确认被拒且未产生执行记录")
}

// 超限：非 UTC 时区下确认时长超过决策最长时长被拒绝
func TestTimezoneExceedsLimit(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "时区药草园")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)

	result := mustEvaluate(t, svc, zone.ID, 600)

	if _, err := svc.ConfirmExecution(result.DecisionID, zone.ID, 601); !errors.Is(err, ErrDurationExceedsLimit) {
		t.Fatalf("expected ErrDurationExceedsLimit, got %v", err)
	}
	if n := countLogs(t, zone.ID, ""); n != 0 {
		t.Fatalf("expected no execution log when duration exceeds limit, got %d", n)
	}
	t.Log("超限场景: 601s 超过决策上限 600s 被拒，未产生执行记录")
}

// 重复使用：非 UTC 时区下同一决策不能确认两次
func TestTimezoneDecisionReused(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "时区茶园")
	mustCreateDevice(t, "pump1", models.DeviceTypePump, zone.ID, models.DeviceStatusOnline)

	result := mustEvaluate(t, svc, zone.ID, 600)

	if _, err := svc.ConfirmExecution(result.DecisionID, zone.ID, 600); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if _, err := svc.ConfirmExecution(result.DecisionID, zone.ID, 600); !errors.Is(err, ErrDecisionAlreadyUsed) {
		t.Fatalf("expected ErrDecisionAlreadyUsed on reused decision, got %v", err)
	}
	if n := countLogs(t, zone.ID, ""); n != 1 {
		t.Fatalf("expected exactly 1 execution log after reused confirm, got %d", n)
	}
	t.Log("重复使用场景: 同一决策二次确认被拒，仅一条执行记录")
}
