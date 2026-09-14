package services

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// 集成测试：每次运行启动独立的 embedded-postgres 实例（独立临时数据目录、动态端口），
// 应用 database/init.sql 建表；无论成功失败，进程与临时目录都会在退出前清理，
// 因此可以连续重复运行。
//
// 测试进程与数据库统一使用非 UTC 时区（testTimeZone），
// 覆盖决策有效期在本地时区下的回归（过期时间曾因 TIMESTAMP 不带时区而偏移）。

// testTimeZone 测试时区：UTC+8 且无夏令时，偏移量足以暴露时区处理错误
const testTimeZone = "Asia/Shanghai"

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	// 测试进程时区：与数据库保持一致，模拟本地时区环境
	loc, err := time.LoadLocation(testTimeZone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load timezone %s: %v\n", testTimeZone, err)
		return 1
	}
	if err := os.Setenv("TZ", testTimeZone); err != nil { // 子进程（initdb/postgres）继承
		fmt.Fprintf(os.Stderr, "set TZ env: %v\n", err)
		return 1
	}
	time.Local = loc

	// 独立临时目录：数据与运行时文件都放在本次运行专属的目录下
	tmpDir, err := os.MkdirTemp("", "decision-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)

	port, err := freePort()
	if err != nil {
		fmt.Fprintf(os.Stderr, "allocate free port: %v\n", err)
		return 1
	}

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(port).
			RuntimePath(filepath.Join(tmpDir, "runtime")).
			DataPath(filepath.Join(tmpDir, "data")),
	)
	if err := pg.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start embedded postgres: %v\n", err)
		return 1
	}
	// defer 在 runTests 返回前执行，成功与失败路径都会停库并清理目录
	defer func() {
		if err := pg.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop embedded postgres: %v\n", err)
		}
	}()

	// TimeZone 运行时参数：连接池内每个会话都固定到测试时区
	dsn := fmt.Sprintf("host=127.0.0.1 port=%d user=postgres password=postgres dbname=postgres sslmode=disable TimeZone=%s",
		port, testTimeZone)
	gormDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect embedded postgres: %v\n", err)
		return 1
	}

	schema, err := os.ReadFile("../../../database/init.sql")
	if err != nil {
		fmt.Fprintf(os.Stderr, "read init.sql: %v\n", err)
		return 1
	}
	if err := gormDB.Exec(string(schema)).Error; err != nil {
		fmt.Fprintf(os.Stderr, "apply init.sql: %v\n", err)
		return 1
	}

	database.DB = gormDB
	return m.Run()
}

// freePort 让内核分配一个空闲端口，避免固定端口在连续或并发运行时冲突
func freePort() (uint32, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return uint32(listener.Addr().(*net.TCPAddr).Port), nil
}

// resetTables 每个用例前清空业务表并重置自增 ID
func resetTables(t *testing.T) {
	t.Helper()
	err := database.DB.Exec(
		"TRUNCATE irrigation_decisions, irrigation_logs, sensor_data, devices, irrigation_zones, system_configs RESTART IDENTITY CASCADE",
	).Error
	if err != nil {
		t.Fatalf("reset tables: %v", err)
	}
}

func mustCreateZone(t *testing.T, name string) models.IrrigationZone {
	t.Helper()
	zone := models.IrrigationZone{Name: name}
	if err := database.DB.Create(&zone).Error; err != nil {
		t.Fatalf("create zone: %v", err)
	}
	return zone
}

func mustCreateDevice(t *testing.T, name string, devType models.DeviceType, zoneID uint, status models.DeviceStatus) models.Device {
	t.Helper()
	device := models.Device{
		Name:         name,
		Type:         devType,
		SerialNumber: fmt.Sprintf("SN-%s-%d", name, time.Now().UnixNano()),
		ZoneID:       &zoneID,
		Status:       status,
	}
	if err := database.DB.Create(&device).Error; err != nil {
		t.Fatalf("create device: %v", err)
	}
	return device
}

func mustAddSensorData(t *testing.T, deviceID uint, dataType string, value float64, ts time.Time) {
	t.Helper()
	data := models.SensorData{DeviceID: deviceID, DataType: dataType, Value: value, Timestamp: ts}
	if err := database.DB.Create(&data).Error; err != nil {
		t.Fatalf("add sensor data: %v", err)
	}
}

func setForecastOverride(t *testing.T, rainfallMm float64) {
	t.Helper()
	cfg := models.SystemConfig{
		Key:   ConfigKeySimulatedForecast,
		Value: fmt.Sprintf("%v", rainfallMm),
	}
	if err := database.DB.Create(&cfg).Error; err != nil {
		t.Fatalf("set forecast override: %v", err)
	}
}

func countLogs(t *testing.T, zoneID uint, status models.ExecutionStatus) int64 {
	t.Helper()
	var count int64
	query := database.DB.Model(&models.IrrigationLog{}).Where("zone_id = ?", zoneID)
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if err := query.Count(&count).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	return count
}

func getLog(t *testing.T, id uint) models.IrrigationLog {
	t.Helper()
	var log models.IrrigationLog
	if err := database.DB.First(&log, id).Error; err != nil {
		t.Fatalf("get log %d: %v", id, err)
	}
	return log
}

// mustEvaluate 评估并持久化一条决策，返回含 decision_id 的决策结果
func mustEvaluate(t *testing.T, svc *DecisionService, zoneID uint, maxDuration int) *DecisionResult {
	t.Helper()
	result, err := svc.Evaluate(DecisionRequest{ZoneID: zoneID, TargetHumidity: 45, MaxDuration: maxDuration})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.DecisionID == 0 {
		t.Fatal("expected decision_id in evaluate result")
	}
	return result
}

func getDecision(t *testing.T, id uint) models.IrrigationDecision {
	t.Helper()
	var decision models.IrrigationDecision
	if err := database.DB.First(&decision, id).Error; err != nil {
		t.Fatalf("get decision %d: %v", id, err)
	}
	return decision
}

// 场景：确认执行时保存本次计划时长，非法确认时长被拒绝且不落记录
func TestConfirmSavesPlannedDuration(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "盆景区")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)
	decision := mustEvaluate(t, svc, zone.ID, 1800)

	// 非正值确认时长 → 拒绝，不落记录，决策保持待确认
	for _, bad := range []int{0, -300} {
		if _, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, bad); !errors.Is(err, ErrInvalidDuration) {
			t.Fatalf("expected ErrInvalidDuration for %d, got %v", bad, err)
		}
	}
	if n := countLogs(t, zone.ID, ""); n != 0 {
		t.Fatalf("expected no execution log for invalid duration, got %d", n)
	}
	if d := getDecision(t, decision.DecisionID); d.Status != models.DecisionStatusPending {
		t.Fatalf("expected decision to stay pending after rejected confirms, got %s", d.Status)
	}

	// 正常确认：计划时长随执行中记录落库，实际时长仍为空，决策被标记为已确认
	log, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 1200)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	persisted := getLog(t, log.ID)
	if persisted.PlannedDuration == nil || *persisted.PlannedDuration != 1200 {
		t.Fatalf("expected planned_duration=1200 persisted, got %v", persisted.PlannedDuration)
	}
	if persisted.Duration != nil {
		t.Fatalf("expected actual duration to be empty before completion, got %v", *persisted.Duration)
	}
	used := getDecision(t, decision.DecisionID)
	if used.Status != models.DecisionStatusConfirmed {
		t.Fatalf("expected decision status=confirmed, got %s", used.Status)
	}
	if used.ConfirmedLogID == nil || *used.ConfirmedLogID != log.ID {
		t.Fatalf("expected decision confirmed_log_id=%d, got %v", log.ID, used.ConfirmedLogID)
	}
	t.Logf("计划时长场景: planned_duration=%ds 已落库，决策 %d 已关联执行记录", *persisted.PlannedDuration, used.ID)
}

// 场景一：正常决策 → 确认执行 → 结束后写回实际时长和估算用水量
func TestDecisionNormalFlow(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "玫瑰园")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)
	soil := mustCreateDevice(t, "soil1", models.DeviceTypeSoilSensor, zone.ID, models.DeviceStatusOnline)
	mustAddSensorData(t, soil.ID, "soil_humidity", 30.0, time.Now())
	setForecastOverride(t, 0) // 未来 24 小时无雨

	// 决策：湿度 30% 低于目标 45%，缺口 15% → 30 分钟（1800 秒）
	result, err := svc.Evaluate(DecisionRequest{ZoneID: zone.ID, TargetHumidity: 45, MaxDuration: 3600})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !result.ShouldIrrigate {
		t.Fatalf("expected should_irrigate=true, got false, reason: %s", result.Reason)
	}
	if result.SuggestedDuration != 1800 {
		t.Fatalf("expected suggested_duration=1800, got %d", result.SuggestedDuration)
	}
	if result.CurrentHumidity == nil || *result.CurrentHumidity != 30.0 {
		t.Fatalf("expected current_humidity=30, got %v", result.CurrentHumidity)
	}
	if result.DecisionID == 0 {
		t.Fatal("expected decision_id in evaluate result")
	}
	if !result.ExpiresAt.After(time.Now()) {
		t.Fatalf("expected decision expires_at in the future, got %v", result.ExpiresAt)
	}
	t.Logf("决策结果: decision_id=%d should_irrigate=%v reason=%q suggested=%ds",
		result.DecisionID, result.ShouldIrrigate, result.Reason, result.SuggestedDuration)

	// 最长时长封顶：max_duration=600 时建议时长应被截断
	capped, err := svc.Evaluate(DecisionRequest{ZoneID: zone.ID, TargetHumidity: 45, MaxDuration: 600})
	if err != nil {
		t.Fatalf("evaluate capped: %v", err)
	}
	if capped.SuggestedDuration != 600 {
		t.Fatalf("expected capped suggested_duration=600, got %d", capped.SuggestedDuration)
	}

	// 确认执行：引用本次决策，创建执行中记录，确认时长保存为计划时长
	log, err := svc.ConfirmExecution(result.DecisionID, zone.ID, result.SuggestedDuration)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if log.Status != models.ExecutionStatusInProgress {
		t.Fatalf("expected status=in_progress, got %s", log.Status)
	}
	persisted := getLog(t, log.ID)
	if persisted.PlannedDuration == nil || *persisted.PlannedDuration != 1800 {
		t.Fatalf("expected planned_duration=1800 persisted, got %v", persisted.PlannedDuration)
	}
	if persisted.Duration != nil {
		t.Fatalf("expected actual duration to be empty before completion, got %v", *persisted.Duration)
	}
	if countLogs(t, zone.ID, models.ExecutionStatusInProgress) != 1 {
		t.Fatal("expected exactly 1 in_progress log")
	}

	// 结束执行：写回实际时长 1500 秒（≤ 计划 1800 秒），估算用水量 150 升
	finished, err := svc.CompleteExecution(log.ID, 1500)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if finished.Status != models.ExecutionStatusSuccess {
		t.Fatalf("expected status=success, got %s", finished.Status)
	}
	if finished.Duration == nil || *finished.Duration != 1500 {
		t.Fatalf("expected duration=1500, got %v", finished.Duration)
	}
	if finished.PlannedDuration == nil || *finished.PlannedDuration != 1800 {
		t.Fatalf("expected planned_duration to stay 1800 after completion, got %v", finished.PlannedDuration)
	}
	if finished.WaterUsage == nil || *finished.WaterUsage != 150.0 {
		t.Fatalf("expected water_usage=150, got %v", finished.WaterUsage)
	}
	if finished.EndTime == nil {
		t.Fatal("expected end_time to be set")
	}
	t.Logf("执行完成: planned=%ds duration=%ds water_usage=%.1fL",
		*finished.PlannedDuration, *finished.Duration, *finished.WaterUsage)

	// 已完结的记录不能重复完结
	if _, err := svc.CompleteExecution(log.ID, 100); !errors.Is(err, ErrLogNotInProgress) {
		t.Fatalf("expected ErrLogNotInProgress, got %v", err)
	}
}

// 场景：超限完成与非正值完成都被拒绝，且原执行中记录保持不变
func TestCompleteExceedsPlanned(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "花坛")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)

	// 确认计划时长 600 秒（决策最长时长 600 秒）
	decision := mustEvaluate(t, svc, zone.ID, 600)
	log, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 600)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}

	assertLogUnchanged := func() {
		t.Helper()
		current := getLog(t, log.ID)
		if current.Status != models.ExecutionStatusInProgress {
			t.Fatalf("expected record to stay in_progress, got %s", current.Status)
		}
		if current.PlannedDuration == nil || *current.PlannedDuration != 600 {
			t.Fatalf("expected planned_duration to stay 600, got %v", current.PlannedDuration)
		}
		if current.Duration != nil {
			t.Fatalf("expected actual duration to stay empty, got %v", *current.Duration)
		}
		if current.WaterUsage != nil {
			t.Fatalf("expected water_usage to stay empty, got %v", *current.WaterUsage)
		}
		if current.EndTime != nil {
			t.Fatalf("expected end_time to stay empty, got %v", *current.EndTime)
		}
	}

	// 超限完成：实际 601 秒 > 计划 600 秒 → 拒绝
	if _, err := svc.CompleteExecution(log.ID, 601); !errors.Is(err, ErrDurationExceedsPlan) {
		t.Fatalf("expected ErrDurationExceedsPlan, got %v", err)
	}
	assertLogUnchanged()

	// 非正值完成：0 和负数 → 拒绝
	for _, bad := range []int{0, -10} {
		if _, err := svc.CompleteExecution(log.ID, bad); !errors.Is(err, ErrInvalidDuration) {
			t.Fatalf("expected ErrInvalidDuration for %d, got %v", bad, err)
		}
	}
	assertLogUnchanged()

	// 边界：实际时长等于计划时长 → 允许
	finished, err := svc.CompleteExecution(log.ID, 600)
	if err != nil {
		t.Fatalf("complete with duration equal to plan should succeed, got %v", err)
	}
	if finished.Duration == nil || *finished.Duration != 600 {
		t.Fatalf("expected duration=600, got %v", finished.Duration)
	}
	t.Log("超限完成场景: 601s 被拒且记录保持执行中，0/-10 被拒，边界 600s 正常完成")
}

// 场景：重复完成被拒绝，首次完成的记录内容保持不变
func TestCompleteDuplicate(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "茶园")
	mustCreateDevice(t, "pump1", models.DeviceTypePump, zone.ID, models.DeviceStatusOnline)

	decision := mustEvaluate(t, svc, zone.ID, 900)
	log, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 900)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// 首次完成：实际 800 秒
	finished, err := svc.CompleteExecution(log.ID, 800)
	if err != nil {
		t.Fatalf("first complete: %v", err)
	}
	if finished.Status != models.ExecutionStatusSuccess {
		t.Fatalf("expected status=success, got %s", finished.Status)
	}

	// 重复完成（含不超上限与超限两种取值）→ 拒绝，且首次完成的结果不变
	for _, again := range []int{800, 999} {
		if _, err := svc.CompleteExecution(log.ID, again); !errors.Is(err, ErrLogNotInProgress) {
			t.Fatalf("expected ErrLogNotInProgress on duplicate complete with %d, got %v", again, err)
		}
	}

	current := getLog(t, log.ID)
	if current.Status != models.ExecutionStatusSuccess {
		t.Fatalf("expected record to stay success, got %s", current.Status)
	}
	if current.Duration == nil || *current.Duration != 800 {
		t.Fatalf("expected duration to stay 800, got %v", current.Duration)
	}
	if current.WaterUsage == nil || *current.WaterUsage != 80.0 {
		t.Fatalf("expected water_usage to stay 80, got %v", current.WaterUsage)
	}
	if current.PlannedDuration == nil || *current.PlannedDuration != 900 {
		t.Fatalf("expected planned_duration to stay 900, got %v", current.PlannedDuration)
	}
	t.Log("重复完成场景: 再次完成被拒，记录保持首次完成的 duration=800s water_usage=80.0L")
}

// 场景二：降雨（近期降雨充足 / 预报有雨 / 湿度已达标）时不建议灌溉
func TestDecisionRainfall(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "草坪区")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)
	soil := mustCreateDevice(t, "soil1", models.DeviceTypeSoilSensor, zone.ID, models.DeviceStatusOnline)
	rain := mustCreateDevice(t, "rain1", models.DeviceTypeRainSensor, zone.ID, models.DeviceStatusOnline)
	mustAddSensorData(t, soil.ID, "soil_humidity", 30.0, time.Now())

	// 2a. 近 24 小时降雨 8mm（≥5mm 阈值）→ 不灌溉
	mustAddSensorData(t, rain.ID, "rainfall", 8.0, time.Now().Add(-3*time.Hour))
	setForecastOverride(t, 0)

	result, err := svc.Evaluate(DecisionRequest{ZoneID: zone.ID, TargetHumidity: 45, MaxDuration: 3600})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.ShouldIrrigate {
		t.Fatalf("expected should_irrigate=false with recent rain, reason: %s", result.Reason)
	}
	if result.RecentRainfall != 8.0 {
		t.Fatalf("expected recent_rainfall=8, got %v", result.RecentRainfall)
	}
	t.Logf("近期降雨场景: %q", result.Reason)

	// 2b. 无近期降雨，但预报未来 24 小时降雨 10mm → 推迟灌溉
	resetTables(t)
	zone = mustCreateZone(t, "草坪区")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)
	soil = mustCreateDevice(t, "soil1", models.DeviceTypeSoilSensor, zone.ID, models.DeviceStatusOnline)
	mustAddSensorData(t, soil.ID, "soil_humidity", 30.0, time.Now())
	setForecastOverride(t, 10)

	result, err = svc.Evaluate(DecisionRequest{ZoneID: zone.ID, TargetHumidity: 45, MaxDuration: 3600})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.ShouldIrrigate {
		t.Fatalf("expected should_irrigate=false with forecast rain, reason: %s", result.Reason)
	}
	if result.ForecastRainfall != 10.0 {
		t.Fatalf("expected forecast_rainfall=10, got %v", result.ForecastRainfall)
	}
	t.Logf("预报降雨场景: %q", result.Reason)

	// 2c. 湿度已达标 → 不灌溉
	resetTables(t)
	zone = mustCreateZone(t, "草坪区")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)
	soil = mustCreateDevice(t, "soil1", models.DeviceTypeSoilSensor, zone.ID, models.DeviceStatusOnline)
	mustAddSensorData(t, soil.ID, "soil_humidity", 50.0, time.Now())
	setForecastOverride(t, 0)

	result, err = svc.Evaluate(DecisionRequest{ZoneID: zone.ID, TargetHumidity: 45, MaxDuration: 3600})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.ShouldIrrigate {
		t.Fatalf("expected should_irrigate=false when humidity reached, reason: %s", result.Reason)
	}
	t.Logf("湿度达标场景: %q", result.Reason)
}

// 场景三：设备不可用时确认执行被拒绝，且不落执行记录、不消耗决策
func TestConfirmDeviceUnavailable(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	// 3a. 区域内阀门离线
	zone := mustCreateZone(t, "蔬菜园")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOffline)
	decision := mustEvaluate(t, svc, zone.ID, 600)

	if _, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 600); !errors.Is(err, ErrDeviceUnavailable) {
		t.Fatalf("expected ErrDeviceUnavailable, got %v", err)
	}
	if n := countLogs(t, zone.ID, ""); n != 0 {
		t.Fatalf("expected no execution log when device unavailable, got %d", n)
	}
	// 决策未被消耗：设备恢复在线后同一决策可确认成功
	mustCreateDevice(t, "valve2", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)
	if _, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 600); err != nil {
		t.Fatalf("confirm with same decision after device recovery should succeed, got %v", err)
	}

	// 3b. 区域完全没有设备
	emptyZone := mustCreateZone(t, "空置区")
	emptyDecision := mustEvaluate(t, svc, emptyZone.ID, 600)
	if _, err := svc.ConfirmExecution(emptyDecision.DecisionID, emptyZone.ID, 600); !errors.Is(err, ErrDeviceUnavailable) {
		t.Fatalf("expected ErrDeviceUnavailable for zone without devices, got %v", err)
	}
	if n := countLogs(t, emptyZone.ID, ""); n != 0 {
		t.Fatalf("expected no execution log for zone without devices, got %d", n)
	}
	t.Log("设备不可用场景: 确认被拒绝且未产生执行记录，决策未被消耗")
}

// 场景四：同一区域已有执行中任务时，新决策的确认被拒绝；完结后可以再次确认
func TestConfirmDuplicate(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "果园")
	mustCreateDevice(t, "pump1", models.DeviceTypePump, zone.ID, models.DeviceStatusOnline)

	d1 := mustEvaluate(t, svc, zone.ID, 900)
	first, err := svc.ConfirmExecution(d1.DecisionID, zone.ID, 900)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}

	// 执行中任务存在时，即使是新的有效决策也被拒绝
	d2 := mustEvaluate(t, svc, zone.ID, 900)
	if _, err := svc.ConfirmExecution(d2.DecisionID, zone.ID, 900); !errors.Is(err, ErrIrrigationInProgress) {
		t.Fatalf("expected ErrIrrigationInProgress while task running, got %v", err)
	}
	if n := countLogs(t, zone.ID, models.ExecutionStatusInProgress); n != 1 {
		t.Fatalf("expected exactly 1 in_progress log, got %d", n)
	}

	// 完结后可以用未被消耗的 d2 再次确认
	if _, err := svc.CompleteExecution(first.ID, 900); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := svc.ConfirmExecution(d2.DecisionID, zone.ID, 900); err != nil {
		t.Fatalf("confirm after complete should succeed, got %v", err)
	}
	t.Log("执行中冲突场景: 执行中拒绝新决策确认，完结后待确认决策仍可使用")
}

// 场景五：并发确认最多只有一个成功，执行中记录恒为一条
func TestConfirmConcurrent(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "果园")
	mustCreateDevice(t, "pump1", models.DeviceTypePump, zone.ID, models.DeviceStatusOnline)

	// 每个并发请求持有各自的有效决策
	const workers = 10
	decisions := make([]*DecisionResult, workers)
	for i := 0; i < workers; i++ {
		decisions[i] = mustEvaluate(t, svc, zone.ID, 600)
	}

	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = svc.ConfirmExecution(decisions[idx].DecisionID, zone.ID, 600)
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrIrrigationInProgress) {
			t.Fatalf("unexpected confirm error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 successful confirm among %d concurrent requests, got %d", workers, succeeded)
	}
	if n := countLogs(t, zone.ID, models.ExecutionStatusInProgress); n != 1 {
		t.Fatalf("expected exactly 1 in_progress log after concurrent confirms, got %d", n)
	}
	// 只有胜出的决策被消耗，其余保持待确认
	var pending int64
	if err := database.DB.Model(&models.IrrigationDecision{}).
		Where("status = ?", models.DecisionStatusPending).Count(&pending).Error; err != nil {
		t.Fatalf("count pending decisions: %v", err)
	}
	if pending != workers-1 {
		t.Fatalf("expected %d decisions to stay pending, got %d", workers-1, pending)
	}
	t.Logf("并发确认场景: %d 个并发确认仅 %d 个成功，执行中记录数=1，未消耗决策=%d", workers, succeeded, pending)
}

// 场景六：缺少区域数据或决策不存在时决策与确认均失败，且不落执行记录
func TestZoneMissing(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	// 评估：区域不存在
	if _, err := svc.Evaluate(DecisionRequest{ZoneID: 999, TargetHumidity: 45, MaxDuration: 600}); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("expected ErrZoneNotFound on evaluate, got %v", err)
	}

	// 确认：决策不存在
	if _, err := svc.ConfirmExecution(99999, 999, 300); !errors.Is(err, ErrDecisionNotFound) {
		t.Fatalf("expected ErrDecisionNotFound on confirm, got %v", err)
	}

	// 确认：区域与决策不一致（决策属于其他区域）
	zone := mustCreateZone(t, "存在的区域")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)
	decision := mustEvaluate(t, svc, zone.ID, 600)
	if _, err := svc.ConfirmExecution(decision.DecisionID, 999, 300); !errors.Is(err, ErrDecisionZoneMismatch) {
		t.Fatalf("expected ErrDecisionZoneMismatch on confirm, got %v", err)
	}

	var count int64
	if err := database.DB.Model(&models.IrrigationLog{}).Count(&count).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no execution log when zone missing or mismatched, got %d", count)
	}
	t.Log("缺少区域数据场景: 评估与确认均被拒绝，未产生执行记录")
}

// 场景：确认时长超过决策最长时长被拒绝，不落记录也不消耗决策，上限内可正常确认
func TestConfirmExceedsDecisionLimit(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "苗圃")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)

	// 决策最长时长 600 秒
	decision := mustEvaluate(t, svc, zone.ID, 600)

	// 超限确认：601 秒 > 决策最长 600 秒 → 拒绝
	if _, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 601); !errors.Is(err, ErrDurationExceedsLimit) {
		t.Fatalf("expected ErrDurationExceedsLimit, got %v", err)
	}
	if n := countLogs(t, zone.ID, ""); n != 0 {
		t.Fatalf("expected no execution log when duration exceeds limit, got %d", n)
	}
	if d := getDecision(t, decision.DecisionID); d.Status != models.DecisionStatusPending {
		t.Fatalf("expected decision to stay pending after rejected confirm, got %s", d.Status)
	}

	// 上限内确认 → 成功
	log, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 600)
	if err != nil {
		t.Fatalf("confirm within limit should succeed, got %v", err)
	}
	if persisted := getLog(t, log.ID); persisted.PlannedDuration == nil || *persisted.PlannedDuration != 600 {
		t.Fatalf("expected planned_duration=600, got %v", persisted.PlannedDuration)
	}
	t.Log("超限确认场景: 601s 被拒且不落记录、决策未消耗，600s 正常确认")
}

// 场景：过期的决策不能用于确认执行
func TestConfirmExpiredDecision(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "温室")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)

	decision := mustEvaluate(t, svc, zone.ID, 600)

	// 将决策过期时间改到过去，模拟过期决策
	if err := database.DB.Model(&models.IrrigationDecision{}).
		Where("id = ?", decision.DecisionID).
		Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("expire decision: %v", err)
	}

	if _, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 300); !errors.Is(err, ErrDecisionExpired) {
		t.Fatalf("expected ErrDecisionExpired, got %v", err)
	}
	if n := countLogs(t, zone.ID, ""); n != 0 {
		t.Fatalf("expected no execution log for expired decision, got %d", n)
	}
	t.Log("过期决策场景: 确认被拒且未产生执行记录")
}

// 场景：同一决策不能重复使用，重复确认被拒绝且只有一条执行中记录
func TestConfirmDecisionReused(t *testing.T) {
	resetTables(t)
	svc := NewDecisionService()

	zone := mustCreateZone(t, "药草园")
	mustCreateDevice(t, "valve1", models.DeviceTypeValve, zone.ID, models.DeviceStatusOnline)

	decision := mustEvaluate(t, svc, zone.ID, 600)

	first, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 600)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}

	// 同一决策再次确认 → 拒绝
	if _, err := svc.ConfirmExecution(decision.DecisionID, zone.ID, 600); !errors.Is(err, ErrDecisionAlreadyUsed) {
		t.Fatalf("expected ErrDecisionAlreadyUsed on reused decision, got %v", err)
	}
	if n := countLogs(t, zone.ID, ""); n != 1 {
		t.Fatalf("expected exactly 1 execution log after reused confirm, got %d", n)
	}

	// 决策状态已确认且关联首次执行记录
	used := getDecision(t, decision.DecisionID)
	if used.Status != models.DecisionStatusConfirmed {
		t.Fatalf("expected decision status=confirmed, got %s", used.Status)
	}
	if used.ConfirmedLogID == nil || *used.ConfirmedLogID != first.ID {
		t.Fatalf("expected confirmed_log_id=%d, got %v", first.ID, used.ConfirmedLogID)
	}
	t.Log("重复确认场景: 同一决策二次确认被拒，仅一条执行记录，决策已关联首次执行")
}
