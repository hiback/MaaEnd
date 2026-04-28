package expressdelivery

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"sync"
	"time"

	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/minicv"
	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

// 小地图可见性识别的调参结果（720p 基准）。
// 色差阈值以欧氏距离（0~442 整数）为量纲；比例参数以 0~100 百分比整数表达。
const (
	minimapCenterX             = 108
	minimapCenterY             = 111
	minimapRadius              = 59
	minimapInnerOffset         = 14
	minimapOuterOffset         = 4
	minimapSampleCount         = 16
	minimapContrastThreshold   = 30
	minimapMotionThreshold     = 3
	minimapContrastPassPercent = 45
	minimapMotionPassPercent   = 80
)

// minimapPresenceStaleThreshold 是上一帧数据的最大有效间隔；
// 超过此值时视为任务切换或长时间暂停，丢弃 prev 避免脏状态污染帧间判定。
const minimapPresenceStaleThreshold = 1500 * time.Millisecond

// minimapDetectionResult 是一次识别的详细结果，用于日志打印与最终 hit 判定。
type minimapDetectionResult struct {
	Hit               bool
	ContrastSub       bool
	MotionSub         bool
	ContrastPassCount int
	MotionStableCount int
	Total             int
	PrevAvailable     bool
}

var _ maa.CustomRecognitionRunner = (*MinimapPresenceRecognition)(nil)

// MinimapPresenceRecognition 基于小地图可见性识别角色是否静止。
//
// 通过「帧间像素稳定性」与「内外圆色差」两个子判定的 AND 组合来判定小地图是否显示。
type MinimapPresenceRecognition struct {
	mu           sync.Mutex
	lastSnapshot []color.RGBA
	lastCalledAt time.Time
}

// Run 执行一次小地图识别：边界检查、（必要时清空）陈旧 prev、
// 调用纯算法函数、打印日志、更新 prev。
//
// 返回 hit=true 当且仅当两个子判定都通过，表示画面显示小地图（角色静止）。
func (r *MinimapPresenceRecognition) Run(_ *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	img := minicv.ImageConvertRGBA(arg.Img)
	if !samplePointsInBounds(img.Bounds()) {
		log.Error().
			Str("component", "ExpressDelivery").
			Str("recognition", "MinimapPresence").
			Int("width", img.Bounds().Dx()).
			Int("height", img.Bounds().Dy()).
			Msg("sample points out of image bounds")
		return nil, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	prev := r.lastSnapshot
	if prev == nil || time.Since(r.lastCalledAt) > minimapPresenceStaleThreshold {
		prev = nil
	}

	snapshot, result := detectMinimapPresence(img, prev)

	r.lastSnapshot = snapshot
	r.lastCalledAt = time.Now()

	log.Info().
		Str("component", "ExpressDelivery").
		Str("recognition", "MinimapPresence").
		Bool("hit", result.Hit).
		Bool("contrast_sub", result.ContrastSub).
		Bool("motion_sub", result.MotionSub).
		Int("contrast_pass", result.ContrastPassCount).
		Int("motion_stable", result.MotionStableCount).
		Int("total", result.Total).
		Bool("prev_available", result.PrevAvailable).
		Msg("minimap presence check")

	recoResult := &maa.CustomRecognitionResult{
		Box: arg.Roi,
		Detail: fmt.Sprintf(
			`{"hit":%t,"contrast_pass":%d,"motion_stable":%d,"total":%d}`,
			result.Hit, result.ContrastPassCount, result.MotionStableCount, result.Total,
		),
	}
	return recoResult, result.Hit
}

// sampleInnerOuter 按 minimapSampleCount 均匀分角度生成内外采样点，
// 并从 img 对应像素位置读取 RGBA 值。调用方需先用 samplePointsInBounds 确保所有点在图像内。
func sampleInnerOuter(img *image.RGBA) (inner, outer []color.RGBA) {
	inner = make([]color.RGBA, minimapSampleCount)
	outer = make([]color.RGBA, minimapSampleCount)
	innerR := float64(minimapRadius - minimapInnerOffset)
	outerR := float64(minimapRadius + minimapOuterOffset)
	for i := 0; i < minimapSampleCount; i++ {
		theta := 2 * math.Pi * float64(i) / float64(minimapSampleCount)
		dx := math.Cos(theta)
		dy := math.Sin(theta)
		ix := int(math.Round(float64(minimapCenterX) + dx*innerR))
		iy := int(math.Round(float64(minimapCenterY) + dy*innerR))
		ox := int(math.Round(float64(minimapCenterX) + dx*outerR))
		oy := int(math.Round(float64(minimapCenterY) + dy*outerR))
		inner[i] = img.RGBAAt(ix, iy)
		outer[i] = img.RGBAAt(ox, oy)
	}
	return inner, outer
}

// samplePointsInBounds 返回 true 当且仅当所有内外采样点都落在 bounds 内。
func samplePointsInBounds(bounds image.Rectangle) bool {
	innerR := float64(minimapRadius - minimapInnerOffset)
	outerR := float64(minimapRadius + minimapOuterOffset)
	for i := 0; i < minimapSampleCount; i++ {
		theta := 2 * math.Pi * float64(i) / float64(minimapSampleCount)
		dx := math.Cos(theta)
		dy := math.Sin(theta)
		ix := int(math.Round(float64(minimapCenterX) + dx*innerR))
		iy := int(math.Round(float64(minimapCenterY) + dy*innerR))
		ox := int(math.Round(float64(minimapCenterX) + dx*outerR))
		oy := int(math.Round(float64(minimapCenterY) + dy*outerR))
		if !(image.Point{X: ix, Y: iy}.In(bounds)) {
			return false
		}
		if !(image.Point{X: ox, Y: oy}.In(bounds)) {
			return false
		}
	}
	return true
}

// rgbDistance 返回两个 RGB 像素的欧氏距离（忽略 alpha）。
// 值域 0 ~ sqrt(255² × 3) ≈ 441.67。
func rgbDistance(a, b color.RGBA) float64 {
	dr := float64(a.R) - float64(b.R)
	dg := float64(a.G) - float64(b.G)
	db := float64(a.B) - float64(b.B)
	return math.Sqrt(dr*dr + dg*dg + db*db)
}

// detectMinimapPresence 基于当前帧与上一帧内点快照评估小地图是否可见。
//
// 调用方必须已经确保所有采样点在 img 内（见 samplePointsInBounds）。
// prev 为上一帧的内点快照；传 nil 表示无上一帧数据，此时 motion 子判定必定为 false。
//
// 返回：
//   - snapshot：本次生成的内点快照，调用方缓存供下一帧使用
//   - result：子判定与总 hit 结果
func detectMinimapPresence(img *image.RGBA, prev []color.RGBA) (snapshot []color.RGBA, result minimapDetectionResult) {
	inner, outer := sampleInnerOuter(img)
	total := minimapSampleCount

	contrastPass := 0
	for i := 0; i < total; i++ {
		if rgbDistance(inner[i], outer[i]) >= float64(minimapContrastThreshold) {
			contrastPass++
		}
	}

	motionStable := 0
	prevAvailable := prev != nil && len(prev) == total
	if prevAvailable {
		for i := 0; i < total; i++ {
			if rgbDistance(inner[i], prev[i]) < float64(minimapMotionThreshold) {
				motionStable++
			}
		}
	}

	// 比例比较用交叉相乘避免浮点除法：
	// (contrastPass / total) >= (contrastPassPercent / 100)
	// 等价于  contrastPass * 100 >= total * contrastPassPercent
	contrastSub := contrastPass*100 >= total*minimapContrastPassPercent
	motionSub := prevAvailable && motionStable*100 >= total*minimapMotionPassPercent

	return inner, minimapDetectionResult{
		Hit:               contrastSub && motionSub,
		ContrastSub:       contrastSub,
		MotionSub:         motionSub,
		ContrastPassCount: contrastPass,
		MotionStableCount: motionStable,
		Total:             total,
		PrevAvailable:     prevAvailable,
	}
}
