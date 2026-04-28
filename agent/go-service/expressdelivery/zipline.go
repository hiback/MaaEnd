package expressdelivery

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/control"
	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

var _ maa.CustomActionRunner = &TraverseZiplineAction{}

type traverseZiplineParams struct {
	StationaryNodeName string `json:"stationary_node_name"`
	PromptNodeName     string `json:"prompt_node_name"`
	PollIntervalMs     int    `json:"poll_interval_ms"`
	DisappearTimeout   int    `json:"disappear_timeout_ms"`
	CompletionTimeout  int    `json:"completion_timeout_ms"`
	StartDelayMs       int    `json:"start_delay_ms"`
	StartMode          string `json:"start_mode"`
	FinishMode         string `json:"finish_mode"`
	MinMovingMs        int    `json:"min_moving_ms"`
	StableGoneMs       int    `json:"stable_gone_ms"`
	StableBackMs       int    `json:"stable_back_ms"`
	StablePromptMs     int    `json:"stable_prompt_ms"`
	ClickX             int    `json:"click_x"`
	ClickY             int    `json:"click_y"`
}

// TraverseZiplineAction starts a zipline hop and waits until arrival is detected.
type TraverseZiplineAction struct{}

func (a *TraverseZiplineAction) Run(ctx *maa.Context, arg *maa.CustomActionArg) bool {
	params := traverseZiplineParams{
		StationaryNodeName: "ExpressDeliveryZiplineStationary",
		PromptNodeName:     "ExpressDeliveryZiplinePrompt",
		PollIntervalMs:     100,
		DisappearTimeout:   5000,
		CompletionTimeout:  15000,
		StartDelayMs:       300,
		StartMode:          "click",
		FinishMode:         finishModeArrival,
		MinMovingMs:        0,
		StableGoneMs:       300,
		StableBackMs:       300,
		StablePromptMs:     100,
		ClickX:             640,
		ClickY:             360,
	}
	if arg != nil && arg.CustomActionParam != "" {
		if err := json.Unmarshal([]byte(arg.CustomActionParam), &params); err != nil {
			log.Error().
				Err(err).
				Str("component", "ExpressDelivery").
				Str("action", "TraverseZipline").
				Msg("failed to parse CustomActionParam")
			return false
		}
	}
	if params.PollIntervalMs <= 0 {
		params.PollIntervalMs = 100
	}
	if params.StartMode == "" {
		params.StartMode = "click"
	}
	if params.FinishMode == "" {
		params.FinishMode = finishModeArrival
	}
	if params.StartDelayMs > 0 {
		time.Sleep(time.Duration(params.StartDelayMs) * time.Millisecond)
	}

	ctrl := ctx.GetTasker().GetController()
	ca, err := control.NewControlAdaptor(ctx, ctrl, 1280, 720)
	if err != nil {
		log.Error().
			Err(err).
			Str("component", "ExpressDelivery").
			Str("action", "TraverseZipline").
			Msg("failed to create control adaptor")
		return false
	}

	if !startZiplineMovement(ca, params) {
		log.Error().
			Str("component", "ExpressDelivery").
			Str("action", "TraverseZipline").
			Str("start_mode", params.StartMode).
			Msg("unsupported zipline start mode")
		return false
	}
	switch params.StartMode {
	case "click":
		movingStart, ok := waitForRecognitionStateStable(ctx, params.StationaryNodeName, false, params.DisappearTimeout, params.PollIntervalMs, params.StableGoneMs, 0)
		if !ok {
			log.Error().
				Str("component", "ExpressDelivery").
				Str("action", "TraverseZipline").
				Msg("zipline stationary prompt did not disappear")
			return false
		}
		if !waitForTraverseFinish(ctx, params, params.CompletionTimeout, params.MinMovingMs-movingStart.elapsedMs()) {
			return false
		}
	case "key_e":
		promptGone, ok := waitForKeyETriggerAccepted(ctx, params)
		if !ok {
			log.Error().
				Str("component", "ExpressDelivery").
				Str("action", "TraverseZipline").
				Msg("zipline prompt did not disappear after pressing E")
			return false
		}
		if !waitForTraverseFinish(ctx, params, params.CompletionTimeout-promptGone.elapsedMs(), 0) {
			return false
		}
	default:
		log.Error().
			Str("component", "ExpressDelivery").
			Str("action", "TraverseZipline").
			Str("start_mode", params.StartMode).
			Msg("unsupported zipline start mode")
		return false
	}
	return true
}

func waitForKeyETriggerAccepted(ctx *maa.Context, params traverseZiplineParams) (stateStableResult, bool) {
	return waitForRecognitionStateStable(ctx, params.PromptNodeName, false, params.DisappearTimeout, params.PollIntervalMs, params.StablePromptMs, params.PollIntervalMs)
}

func startZiplineMovement(ca control.ControlAdaptor, params traverseZiplineParams) bool {
	switch params.StartMode {
	case "click":
		ca.TouchClick(0, params.ClickX, params.ClickY, 80, 0)
		return true
	case "key_e":
		ca.KeyType(keyCodeE, 0)
		return true
	default:
		return false
	}
}

func waitForTraverseFinish(ctx *maa.Context, params traverseZiplineParams, timeoutMs, minWaitMs int) bool {
	switch params.FinishMode {
	case finishModeArrival:
		if _, ok := waitForRecognitionStateStable(ctx, params.StationaryNodeName, true, timeoutMs, params.PollIntervalMs, params.StableBackMs, minWaitMs); !ok {
			log.Error().
				Str("component", "ExpressDelivery").
				Str("action", "TraverseZipline").
				Str("finish_mode", params.FinishMode).
				Msg("zipline stationary prompt was not detected")
			return false
		}
		return true
	case finishModePrompt:
		if _, ok := waitForRecognitionStateStable(ctx, params.PromptNodeName, true, timeoutMs, params.PollIntervalMs, params.StablePromptMs, minWaitMs); !ok {
			log.Error().
				Str("component", "ExpressDelivery").
				Str("action", "TraverseZipline").
				Str("finish_mode", params.FinishMode).
				Msg("next zipline prompt was not detected")
			return false
		}
		return true
	default:
		log.Error().
			Str("component", "ExpressDelivery").
			Str("action", "TraverseZipline").
			Str("finish_mode", params.FinishMode).
			Msg("unsupported zipline finish mode")
		return false
	}
}

type stateStableResult struct {
	matchedAt time.Time
	startAt   time.Time
}

func (r stateStableResult) elapsedMs() int {
	if r.startAt.IsZero() || r.matchedAt.IsZero() {
		return 0
	}
	return int(r.matchedAt.Sub(r.startAt).Milliseconds())
}

func waitForRecognitionStateStable(ctx *maa.Context, nodeName string, expected bool, timeoutMs, pollIntervalMs, stableMs, minWaitMs int) (stateStableResult, bool) {
	startAt := time.Now()
	deadline := startAt.Add(time.Duration(timeoutMs) * time.Millisecond)
	var matchedSince time.Time
	for time.Now().Before(deadline) {
		if ctx.GetTasker().Stopping() {
			return stateStableResult{}, false
		}
		if minWaitMs > 0 && time.Since(startAt) < time.Duration(minWaitMs)*time.Millisecond {
			time.Sleep(time.Duration(pollIntervalMs) * time.Millisecond)
			continue
		}
		hit, err := runRecognitionHit(ctx, nodeName)
		if err == nil && hit == expected {
			if matchedSince.IsZero() {
				matchedSince = time.Now()
			} else if time.Since(matchedSince) >= time.Duration(stableMs)*time.Millisecond {
				return stateStableResult{matchedAt: time.Now(), startAt: startAt}, true
			}
		} else {
			matchedSince = time.Time{}
		}
		time.Sleep(time.Duration(pollIntervalMs) * time.Millisecond)
	}
	return stateStableResult{}, false
}

func runRecognitionHit(ctx *maa.Context, nodeName string) (bool, error) {
	ctrl := ctx.GetTasker().GetController()
	ctrl.PostScreencap().Wait()
	img, err := ctrl.CacheImage()
	if err != nil {
		return false, fmt.Errorf("cache image: %w", err)
	}
	detail, err := ctx.RunRecognition(nodeName, img)
	if err != nil {
		return false, err
	}
	if detail == nil {
		return false, nil
	}
	return detail.Hit, nil
}

const keyCodeE = 0x45

const (
	finishModeArrival = "arrival"
	finishModePrompt  = "prompt"
)
