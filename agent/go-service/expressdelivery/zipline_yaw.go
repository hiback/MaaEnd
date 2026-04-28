package expressdelivery

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	maptracker "github.com/MaaXYZ/MaaEnd/agent/go-service/map-tracker"
	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

const (
	ziplineYawDefaultUnitsPerDegree = 2.0
	ziplineYawMinUnitsPerDegree     = 1.0
	ziplineYawMaxUnitsPerDegree     = 6.0
	ziplineYawStableMinRotConf      = 0.55
	ziplineYawAdaptiveMinCommand    = 15.0
	ziplineYawAdaptiveMinObserved   = 10.0
	ziplineYawInferRetryCount       = 5
	ziplineYawRetryDelayMs          = 120
	ziplineYawCenterX               = 1280 / 2
	ziplineYawCenterY               = 720 / 2
	ziplineYawResetDurationMs       = 16

	// ziplineMaxAngularVelocityDPS is the in-game maximum turn rate on the
	// zipline — 180° takes ~0.7s, so ~260°/s. Settle time is computed directly
	// from this: the character cannot turn faster than this rate no matter how
	// large the mouse delta is.
	ziplineMaxAngularVelocityDPS = 260.0
	ziplineSettleSafetyMs        = 150
	ziplineSwipeDurationMs       = 30

	// Bootstrap: on a fresh unitsPerDegree estimator, cap per-step commanded
	// angle so every sample is a "fully settled" one and the EMA converges
	// quickly on machines whose mouse sensitivity differs from the default.
	ziplineYawProbeMaxDeg      = 45
	ziplineYawBootstrapSamples = 3
	ziplineYawBootstrapAlpha   = 0.7
	ziplineYawSteadyAlpha      = 0.2

	// Steady-state iteration budget: first shot + at most one correction.
	ziplineYawSteadyMaxIters = 2
)

// ziplineYawScaleEstimator tracks cursor-pixels-per-degree (the "units per
// degree" scale) for hover-only yaw rotation on the zipline. unitsPerDegree is
// physically constant for a given machine (determined by Windows mouse
// sensitivity + DPI + in-game sensitivity), so we only need to learn it once
// per process. A short bootstrap phase uses a large EMA alpha + probe-capped
// commands to converge in ~3 samples; after that the estimate is treated as
// fixed and the gain only drifts slightly during corrective shots.
type ziplineYawScaleEstimator struct {
	unitsPerDegree    float64
	minUnitsPerDegree float64
	maxUnitsPerDegree float64
	bootstrapAlpha    float64
	steadyAlpha       float64
	minCommandDeg     float64
	minObservedDeg    float64
	sampleCount       int
	bootstrapTarget   int
}

func newZiplineYawScaleEstimator() *ziplineYawScaleEstimator {
	return &ziplineYawScaleEstimator{
		unitsPerDegree:    ziplineYawDefaultUnitsPerDegree,
		minUnitsPerDegree: ziplineYawMinUnitsPerDegree,
		maxUnitsPerDegree: ziplineYawMaxUnitsPerDegree,
		bootstrapAlpha:    ziplineYawBootstrapAlpha,
		steadyAlpha:       ziplineYawSteadyAlpha,
		minCommandDeg:     ziplineYawAdaptiveMinCommand,
		minObservedDeg:    ziplineYawAdaptiveMinObserved,
		bootstrapTarget:   ziplineYawBootstrapSamples,
	}
}

// ziplineYawScale is sticky across alignZiplineYaw calls so the learned
// units-per-degree persists for the entire go-service process lifetime. Mouse
// sensitivity does not change mid-session, so one bootstrap pays for every
// subsequent zipline turn.
var ziplineYawScale = newZiplineYawScaleEstimator()

func (e *ziplineYawScaleEstimator) UnitsPerDegree() float64 {
	return e.unitsPerDegree
}

func (e *ziplineYawScaleEstimator) InBootstrap() bool {
	return e.sampleCount < e.bootstrapTarget
}

func (e *ziplineYawScaleEstimator) SampleCount() int {
	return e.sampleCount
}

func (e *ziplineYawScaleEstimator) DegreesToUnits(deltaDeg int) int {
	return int(math.Round(float64(deltaDeg) * e.unitsPerDegree))
}

// UpdateFromSample folds one commanded/observed pair into the EMA estimate.
// Returns previous/next scale and whether the sample was accepted — callers
// use this for logging. The alpha depends on bootstrap state.
func (e *ziplineYawScaleEstimator) UpdateFromSample(commandedDeg, observedDeg int) (prev, next float64, accepted bool) {
	prev = e.unitsPerDegree
	if math.Abs(float64(commandedDeg)) < e.minCommandDeg ||
		math.Abs(float64(observedDeg)) < e.minObservedDeg {
		return prev, prev, false
	}
	sample := math.Abs(float64(commandedDeg)) * e.unitsPerDegree / math.Abs(float64(observedDeg))
	if sample < e.minUnitsPerDegree || sample > e.maxUnitsPerDegree {
		return prev, prev, false
	}
	alpha := e.steadyAlpha
	if e.InBootstrap() {
		alpha = e.bootstrapAlpha
	}
	next = prev*(1.0-alpha) + sample*alpha
	if next < e.minUnitsPerDegree {
		next = e.minUnitsPerDegree
	} else if next > e.maxUnitsPerDegree {
		next = e.maxUnitsPerDegree
	}
	e.unitsPerDegree = next
	e.sampleCount++
	return prev, next, true
}

func alignZiplineYaw(ctx *maa.Context, targetYaw int) bool {
	targetRot := normalizeZiplineRotation(targetYaw)
	scale := ziplineYawScale

	result, err := inferZiplineRotation(ctx)
	if err != nil {
		log.Error().
			Err(err).
			Str("component", "ExpressDelivery").
			Str("action", "AlignZiplineYaw").
			Msg("failed initial rotation inference")
		return false
	}

	maxIters := ziplineYawSteadyMaxIters
	if scale.InBootstrap() {
		// Worst case cold start: initial unitsPerDegree may be off by 2x,
		// so each probe step may only achieve half its cap in actual rotation.
		// Budget accordingly, plus one correction after convergence.
		maxIters = int(math.Ceil(180.0/float64(ziplineYawProbeMaxDeg/2))) + 1
	}

	for iteration := 0; iteration < maxIters; iteration++ {
		delta := calcSignedZiplineDeltaRotation(result.Rot, targetRot)
		absDelta := int(math.Abs(float64(delta)))
		if absDelta <= expressDeliveryAlignThreshold {
			log.Debug().
				Str("component", "ExpressDelivery").
				Str("action", "AlignZiplineYaw").
				Int("iteration", iteration+1).
				Int("current_rot", result.Rot).
				Int("target_rot", targetRot).
				Int("delta_rot", delta).
				Int("sample_count", scale.SampleCount()).
				Bool("bootstrap", scale.InBootstrap()).
				Msg("aligned")
			return true
		}

		// Decide step size and whether this shot is allowed to update gain.
		// Bootstrap: cap to probe size, always learn (every sample is clean).
		// Steady: one-shot full delta; only learn on corrective iterations so
		// the first (uncertain) shot never pollutes a known-good gain.
		var stepDelta int
		var allowLearn bool
		if scale.InBootstrap() {
			stepDelta = clampZiplineProbeDelta(delta, ziplineYawProbeMaxDeg)
			allowLearn = true
		} else {
			stepDelta = delta
			allowLearn = iteration > 0
		}

		// Every swipe starts from screen center — no cursor drift, no clamping
		// risk, and no need for a deferred recenter.
		dx := scale.DegreesToUnits(stepDelta)
		settleMs := estimateZiplineSettleMs(stepDelta)
		if !rotateZiplineViewHoverOnly(ctx, ziplineYawCenterX, ziplineYawCenterY, dx, 0, ziplineSwipeDurationMs) {
			log.Error().
				Str("component", "ExpressDelivery").
				Str("action", "AlignZiplineYaw").
				Int("iteration", iteration+1).
				Int("dx", dx).
				Msg("failed to run hover-only yaw rotation")
			return false
		}

		endCursorX := ziplineYawCenterX + dx
		log.Debug().
			Str("component", "ExpressDelivery").
			Str("action", "AlignZiplineYaw").
			Int("iteration", iteration+1).
			Int("current_rot", result.Rot).
			Int("target_rot", targetRot).
			Int("delta_rot", delta).
			Int("step_delta_rot", stepDelta).
			Int("cursor_x", endCursorX).
			Int("dx", dx).
			Int("settle_ms", settleMs).
			Float64("rot_speed", scale.UnitsPerDegree()).
			Int("sample_count", scale.SampleCount()).
			Bool("bootstrap", scale.InBootstrap()).
			Msg("rotating toward target rotation")

		prevRot := result.Rot
		result, err = sleepAndInferZiplineRotation(ctx, settleMs)
		if err != nil {
			log.Error().
				Err(err).
				Str("component", "ExpressDelivery").
				Str("action", "AlignZiplineYaw").
				Int("iteration", iteration+1).
				Int("previous_rot", prevRot).
				Int("commanded_delta_rot", stepDelta).
				Msg("failed to infer rotation after settle")
			return false
		}
		if result.InferMode == "VirtualHit" || result.RotConf < ziplineYawStableMinRotConf {
			log.Warn().
				Str("component", "ExpressDelivery").
				Str("action", "AlignZiplineYaw").
				Int("iteration", iteration+1).
				Str("infer_mode", result.InferMode).
				Float64("rot_conf", result.RotConf).
				Msg("dropping gain sample due to low-confidence or virtual-hit rotation inference")
			allowLearn = false
		}

		// Recenter cursor before next iteration (or before function returns).
		// Alt-key prevents the recenter swipe from rotating the view.
		if endCursorX != ziplineYawCenterX {
			if !resetZiplineHoverToCenter(ctx, endCursorX, ziplineYawCenterY) {
				log.Warn().
					Str("component", "ExpressDelivery").
					Str("action", "AlignZiplineYaw").
					Int("iteration", iteration+1).
					Int("cursor_x", endCursorX).
					Msg("failed to recenter hover cursor after rotation step")
			}
		}

		if allowLearn {
			actualDelta := calcSignedZiplineDeltaRotation(prevRot, result.Rot)
			if prev, next, accepted := scale.UpdateFromSample(stepDelta, actualDelta); accepted {
				log.Debug().
					Str("component", "ExpressDelivery").
					Str("action", "AlignZiplineYaw").
					Int("iteration", iteration+1).
					Int("commanded_rot", stepDelta).
					Int("observed_rot", actualDelta).
					Float64("old_rot_speed", prev).
					Float64("new_rot_speed", next).
					Int("sample_count", scale.SampleCount()).
					Bool("bootstrap", scale.InBootstrap()).
					Msg("adaptive rotation speed updated")
			}
		}
	}

	finalDelta := calcSignedZiplineDeltaRotation(result.Rot, targetRot)
	if int(math.Abs(float64(finalDelta))) <= expressDeliveryAlignThreshold {
		return true
	}

	log.Error().
		Str("component", "ExpressDelivery").
		Str("action", "AlignZiplineYaw").
		Int("current_rot", result.Rot).
		Int("target_rot", targetRot).
		Int("delta_rot", finalDelta).
		Int("max_iterations", maxIters).
		Bool("bootstrap", scale.InBootstrap()).
		Msg("failed to align to target rotation within retry budget")
	return false
}

// Zipline alignment must not emit clicks, so it uses ExpressDelivery-owned
// hover-only Pipeline actions instead of CharacterController actions or direct
// controller calls.
func rotateZiplineViewHoverOnly(ctx *maa.Context, beginX, beginY, dx, dy, durationMs int) bool {
	forward := map[string]any{
		"__ExpressDeliveryZiplineHoverSwipeAction": map[string]any{
			"begin":    maa.Rect{beginX, beginY, 4, 4},
			"end":      maa.Rect{beginX + dx, beginY + dy, 4, 4},
			"duration": durationMs,
		},
	}
	ctx.RunAction("__ExpressDeliveryZiplineHoverSwipeAction",
		maa.Rect{0, 0, 0, 0}, "", forward)
	return true
}

// estimateZiplineSettleMs returns how long to wait for the character yaw to
// catch up to the commanded mouse delta. The in-game turn rate is bounded by
// ziplineMaxAngularVelocityDPS, so settle = |commanded| / rate + safety. The
// swipe duration itself is irrelevant: the character continues rotating toward
// the mouse endpoint for as long as it takes to reach it, even after the swipe
// has ended.
func estimateZiplineSettleMs(commandedDeg int) int {
	rotateMs := int(math.Ceil(math.Abs(float64(commandedDeg)) / ziplineMaxAngularVelocityDPS * 1000.0))
	return rotateMs + ziplineSettleSafetyMs
}

func clampZiplineProbeDelta(delta, capDeg int) int {
	if delta > capDeg {
		return capDeg
	}
	if delta < -capDeg {
		return -capDeg
	}
	return delta
}

func resetZiplineHoverToCenter(ctx *maa.Context, beginX, beginY int) bool {
	ctx.RunAction("__ExpressDeliveryZiplineAltKeyDownAction",
		maa.Rect{0, 0, 0, 0}, "", nil)

	backward := map[string]any{
		"__ExpressDeliveryZiplineHoverSwipeAction": map[string]any{
			"begin":    maa.Rect{beginX, beginY, 4, 4},
			"end":      maa.Rect{ziplineYawCenterX, ziplineYawCenterY, 4, 4},
			"duration": ziplineYawResetDurationMs,
		},
	}
	ctx.RunAction("__ExpressDeliveryZiplineHoverSwipeAction",
		maa.Rect{0, 0, 0, 0}, "", backward)
	ctx.RunAction("__ExpressDeliveryZiplineAltKeyUpAction",
		maa.Rect{0, 0, 0, 0}, "", nil)
	return true
}

func inferZiplineRotation(ctx *maa.Context) (*maptracker.MapTrackerInferResult, error) {
	paramBytes, err := json.Marshal(map[string]any{
		"map_name_regex": expressDeliveryMapNameRegex,
		"precision":      1.0,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal infer params: %w", err)
	}

	taskDetail, err := ctx.GetTaskJob().GetDetail()
	if err != nil {
		return nil, fmt.Errorf("get task detail: %w", err)
	}
	ctrl := ctx.GetTasker().GetController()
	var lastErr error
	for attempt := 0; attempt < ziplineYawInferRetryCount; attempt++ {
		ctrl.PostScreencap().Wait()
		img, err := ctrl.CacheImage()
		if err != nil {
			lastErr = fmt.Errorf("cache image: %w", err)
			time.Sleep(ziplineYawRetryDelayMs * time.Millisecond)
			continue
		}

		resultWrapper, hit := (&maptracker.MapTrackerInfer{}).Run(ctx, &maa.CustomRecognitionArg{
			TaskID:                 taskDetail.ID,
			CurrentTaskName:        taskDetail.Entry,
			CustomRecognitionName:  "MapTrackerInfer",
			CustomRecognitionParam: string(paramBytes),
			Img:                    img,
			Roi:                    maa.Rect{0, 0, 0, 0},
		})
		if !hit || resultWrapper == nil || resultWrapper.Detail == "" {
			lastErr = fmt.Errorf("MapTrackerInfer did not hit")
			time.Sleep(ziplineYawRetryDelayMs * time.Millisecond)
			continue
		}

		var result maptracker.MapTrackerInferResult
		if err := json.Unmarshal([]byte(resultWrapper.Detail), &result); err != nil {
			lastErr = fmt.Errorf("unmarshal infer result: %w", err)
			time.Sleep(ziplineYawRetryDelayMs * time.Millisecond)
			continue
		}
		return &result, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("MapTrackerInfer did not hit")
	}
	return nil, lastErr
}

func sleepAndInferZiplineRotation(ctx *maa.Context, settleMs int) (*maptracker.MapTrackerInferResult, error) {
	if settleMs > 0 {
		time.Sleep(time.Duration(settleMs) * time.Millisecond)
	}
	return inferZiplineRotation(ctx)
}

func normalizeZiplineRotation(rot int) int {
	rot %= 360
	if rot < 0 {
		rot += 360
	}
	return rot
}

func calcSignedZiplineDeltaRotation(current, target int) int {
	delta := normalizeZiplineRotation(target) - normalizeZiplineRotation(current)
	if delta > 180 {
		delta -= 360
	}
	if delta < -180 {
		delta += 360
	}
	return delta
}
