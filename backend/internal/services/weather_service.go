package services

import (
	"math"
	"strconv"
	"strings"
	"time"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// ConfigKeySimulatedForecast 系统配置键：覆盖模拟预报降雨量（mm），便于演示与测试
const ConfigKeySimulatedForecast = "simulated_forecast_rainfall"

// RainForecast 未来 24 小时降雨预报
type RainForecast struct {
	ExpectedRainfall float64 `json:"expected_rainfall"` // 预计降雨量（mm）
	Probability      float64 `json:"probability"`       // 降雨概率 0~1
	Description      string  `json:"description"`       // 预报说明
}

// ForecastProvider 天气预报接口，便于替换为真实天气服务或测试桩
type ForecastProvider interface {
	GetRainForecast(zoneID uint) (*RainForecast, error)
}

// SimulatedForecastProvider 模拟天气预报（需求约定预报数据为模拟数据）。
// 若 system_configs 中存在 simulated_forecast_rainfall 配置，优先使用该值；
// 否则按日期与区域确定性地生成 0~8mm 的预报降雨量。
type SimulatedForecastProvider struct{}

func NewSimulatedForecastProvider() *SimulatedForecastProvider {
	return &SimulatedForecastProvider{}
}

func (p *SimulatedForecastProvider) GetRainForecast(zoneID uint) (*RainForecast, error) {
	var cfg models.SystemConfig
	if err := database.DB.Where("key = ?", ConfigKeySimulatedForecast).First(&cfg).Error; err == nil {
		if v, perr := strconv.ParseFloat(strings.TrimSpace(cfg.Value), 64); perr == nil && v >= 0 {
			return &RainForecast{
				ExpectedRainfall: v,
				Probability:      rainProbability(v),
				Description:      "模拟预报（系统配置覆盖）",
			}, nil
		}
	}

	// 确定性模拟：按一年中的日期与区域偏移生成 0~8mm 预报降雨量
	dayOfYear := float64(time.Now().YearDay())
	rainfall := 4.0 + 4.0*math.Sin(dayOfYear*0.37+float64(zoneID)*1.31)
	rainfall = math.Round(rainfall*10) / 10

	return &RainForecast{
		ExpectedRainfall: rainfall,
		Probability:      rainProbability(rainfall),
		Description:      "模拟预报",
	}, nil
}

func rainProbability(rainfallMm float64) float64 {
	return math.Min(0.95, 0.2+rainfallMm/10.0)
}
