package expressdelivery

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/control"
	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/maafocus"
	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

var _ maa.CustomActionRunner = &RouteAction{}

type routeParams struct {
	Route      string `json:"route"`
	TargetName string `json:"target_name"`
}

type routeStep struct {
	Kind   string
	Yaw    int
	Count  int
	HasYaw bool
}

const (
	routeStepMove                  = "move"
	routeStepNext                  = "next"
	routeStepEnd                   = "end"
	routeZiplineTimeoutMs          = 9000
	routeZiplineLongTimeoutMs      = 12000
	expressDeliveryMapNameRegex    = "^map02_lv002(_tier_\\w+)?$"
	expressDeliveryAlignThreshold  = 5
	expressDeliveryStepStartDelay  = 300
	expressDeliveryStepPollMs      = 100
	expressDeliveryStepStableGone  = 300
	expressDeliveryStepStableBack  = 300
	expressDeliveryStepStartWaitMs = 5000
)

// RouteAction executes a user-configured zipline route for the detected target.
type RouteAction struct{}

func (a *RouteAction) Run(ctx *maa.Context, arg *maa.CustomActionArg) bool {
	params, err := parseRouteParams(arg)
	if err != nil {
		maafocus.Print(ctx, "快递配送滑索路线参数解析失败")
		log.Error().
			Err(err).
			Str("component", "ExpressDelivery").
			Str("action", "RouteAction").
			Msg("failed to parse route params")
		return false
	}

	steps, err := parseRoute(params.Route)
	if err != nil {
		maafocus.Print(ctx, fmt.Sprintf("%s滑索路线格式错误：%v", displayTargetName(params.TargetName), err))
		log.Error().
			Err(err).
			Str("component", "ExpressDelivery").
			Str("action", "RouteAction").
			Str("target", displayTargetName(params.TargetName)).
			Str("route", params.Route).
			Msg("invalid route expression")
		return false
	}

	targetName := displayTargetName(params.TargetName)
	if len(steps) == 0 {
		maafocus.Print(ctx, fmt.Sprintf("未配置%s滑索路线指令，任务已停止", targetName))
		log.Error().
			Str("component", "ExpressDelivery").
			Str("action", "RouteAction").
			Str("target", targetName).
			Msg("route command is empty")
		return false
	}
	maafocus.Print(ctx, fmt.Sprintf("开始执行%s滑索路线，共 %d 步", targetName, len(steps)))

	for idx, step := range steps {
		var prevStep *routeStep
		if idx > 0 {
			prevStep = &steps[idx-1]
		}
		var nextStep *routeStep
		if idx+1 < len(steps) {
			nextStep = &steps[idx+1]
		}
		switch step.Kind {
		case routeStepMove:
			if !runMoveStep(ctx, targetName, idx, len(steps), step.Yaw, prevStep, nextStep) {
				return false
			}
		case routeStepNext:
			if !runNextStep(ctx, targetName, idx, len(steps), step.Count, prevStep, nextStep) {
				return false
			}
		case routeStepEnd:
			if !runEndStep(ctx, targetName, idx, len(steps), step) {
				return false
			}
		default:
			maafocus.Print(ctx, fmt.Sprintf("%s滑索路线包含未知步骤：%s", targetName, step.Kind))
			log.Error().
				Str("component", "ExpressDelivery").
				Str("action", "RouteAction").
				Str("target", targetName).
				Str("step_kind", step.Kind).
				Msg("route contains unknown step")
			return false
		}
	}

	maafocus.Print(ctx, fmt.Sprintf("已完成%s滑索路线，当前滑索架视为终点", targetName))
	return true
}

func parseRouteParams(arg *maa.CustomActionArg) (*routeParams, error) {
	if arg == nil || arg.CustomActionParam == "" {
		return nil, fmt.Errorf("custom_action_param is empty")
	}

	var params routeParams
	if err := json.Unmarshal([]byte(arg.CustomActionParam), &params); err != nil {
		return nil, err
	}
	return &params, nil
}

func parseRoute(route string) ([]routeStep, error) {
	trimmed := strings.TrimSpace(route)
	if trimmed == "" {
		return []routeStep{}, nil
	}

	parts := strings.Split(trimmed, ",")
	steps := make([]routeStep, 0, len(parts))
	for idx, raw := range parts {
		part := strings.TrimSpace(raw)
		if part == "" {
			return nil, fmt.Errorf("第 %d 段为空", idx+1)
		}

		items := strings.Split(part, ":")
		switch items[0] {
		case routeStepMove:
			if len(items) != 2 {
				return nil, fmt.Errorf("第 %d 段 move 格式应为 move:<yaw>", idx+1)
			}
			yaw, err := strconv.Atoi(items[1])
			if err != nil || yaw < 0 || yaw > 359 {
				return nil, fmt.Errorf("第 %d 段 move 角度必须为 0-359 的整数", idx+1)
			}
			steps = append(steps, routeStep{Kind: routeStepMove, Yaw: yaw})
		case routeStepNext:
			if len(items) != 2 {
				return nil, fmt.Errorf("第 %d 段 next 格式应为 next:<count>", idx+1)
			}
			count, err := strconv.Atoi(items[1])
			if err != nil || count <= 0 {
				return nil, fmt.Errorf("第 %d 段 next 次数必须为正整数", idx+1)
			}
			steps = append(steps, routeStep{Kind: routeStepNext, Count: count})
		case routeStepEnd:
			switch len(items) {
			case 1:
				steps = append(steps, routeStep{Kind: routeStepEnd})
			case 2:
				yaw, err := strconv.Atoi(items[1])
				if err != nil || yaw < 0 || yaw > 359 {
					return nil, fmt.Errorf("第 %d 段 end 角度必须为 0-359 的整数", idx+1)
				}
				steps = append(steps, routeStep{Kind: routeStepEnd, Yaw: yaw, HasYaw: true})
			default:
				return nil, fmt.Errorf("第 %d 段 end 格式应为 end 或 end:<yaw>", idx+1)
			}
		default:
			return nil, fmt.Errorf("第 %d 段包含未知指令 %q", idx+1, items[0])
		}
	}

	if steps[0].Kind != routeStepMove {
		return nil, fmt.Errorf("首段必须为 move:<yaw>")
	}
	if steps[len(steps)-1].Kind != routeStepEnd {
		return nil, fmt.Errorf("末段必须为 end 或 end:<yaw>")
	}
	for i := 0; i < len(steps)-1; i++ {
		if steps[i].Kind == routeStepEnd {
			return nil, fmt.Errorf("第 %d 段 end 只能出现在末段", i+1)
		}
	}
	return steps, nil
}

func runMoveStep(ctx *maa.Context, targetName string, idx, total, yaw int, prevStep, nextStep *routeStep) bool {
	maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d：move:%d", targetName, idx+1, total, yaw))
	if !alignZiplineYaw(ctx, yaw) {
		maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d 失败：朝向校正到 %d 度失败", targetName, idx+1, total, yaw))
		log.Error().
			Str("component", "ExpressDelivery").
			Str("action", "RouteAction").
			Str("target", targetName).
			Int("step_index", idx+1).
			Int("yaw", yaw).
			Msg("failed to align yaw for move step")
		return false
	}
	timeoutMs := calcStepTimeoutMs(prevStep, nextStep)
	finishMode := calcFinishMode(nextStep)
	if !runTraverse(ctx, "click", finishMode, timeoutMs) {
		maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d 失败：move:%d 未在 %d 秒内%s", targetName, idx+1, total, yaw, timeoutMs/1000, finishModeFailureText(finishMode)))
		log.Error().
			Str("component", "ExpressDelivery").
			Str("action", "RouteAction").
			Str("target", targetName).
			Int("step_index", idx+1).
			Int("yaw", yaw).
			Int("timeout_ms", timeoutMs).
			Str("finish_mode", finishMode).
			Msg("move step zipline traversal failed")
		return false
	}
	return true
}

func runNextStep(ctx *maa.Context, targetName string, idx, total, count int, prevStep, nextStep *routeStep) bool {
	maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d：next:%d", targetName, idx+1, total, count))
	for hop := 1; hop <= count; hop++ {
		hopPrev := prevStep
		if hop > 1 {
			hopPrev = &routeStep{Kind: routeStepNext}
		}
		hopNext := nextStep
		if hop < count {
			hopNext = &routeStep{Kind: routeStepNext}
		}
		timeoutMs := calcStepTimeoutMs(hopPrev, hopNext)
		finishMode := calcFinishMode(hopNext)
		maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d：默认滑索 %d/%d", targetName, idx+1, total, hop, count))
		if !runTraverse(ctx, "key_e", finishMode, timeoutMs) {
			maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d 失败：next:%d 的第 %d 次未在 %d 秒内%s", targetName, idx+1, total, count, hop, timeoutMs/1000, finishModeFailureText(finishMode)))
			log.Error().
				Str("component", "ExpressDelivery").
				Str("action", "RouteAction").
				Str("target", targetName).
				Int("step_index", idx+1).
				Int("count", count).
				Int("hop", hop).
				Int("timeout_ms", timeoutMs).
				Str("finish_mode", finishMode).
				Msg("default zipline hop failed")
			return false
		}
	}
	return true
}

func runEndStep(ctx *maa.Context, targetName string, idx, total int, step routeStep) bool {
	if step.HasYaw {
		maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d：end:%d", targetName, idx+1, total, step.Yaw))
		if !alignZiplineYaw(ctx, step.Yaw) {
			maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d 失败：朝向校正到 %d 度失败", targetName, idx+1, total, step.Yaw))
			log.Error().
				Str("component", "ExpressDelivery").
				Str("action", "RouteAction").
				Str("target", targetName).
				Int("step_index", idx+1).
				Int("yaw", step.Yaw).
				Msg("failed to align yaw for end step")
			return false
		}
	} else {
		maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d：end", targetName, idx+1, total))
	}
	if !runRightClickDrop(ctx) {
		maafocus.Print(ctx, fmt.Sprintf("%s滑索路线 %d/%d 失败：右键下滑索执行失败", targetName, idx+1, total))
		log.Error().
			Str("component", "ExpressDelivery").
			Str("action", "RouteAction").
			Str("target", targetName).
			Int("step_index", idx+1).
			Msg("failed to perform right click drop at end step")
		return false
	}
	return true
}

func runRightClickDrop(ctx *maa.Context) bool {
	ctrl := ctx.GetTasker().GetController()
	ca, err := control.NewControlAdaptor(ctx, ctrl, 1280, 720)
	if err != nil {
		log.Error().
			Err(err).
			Str("component", "ExpressDelivery").
			Str("action", "RouteAction").
			Msg("failed to create control adaptor for end step")
		return false
	}
	ca.TouchClick(1, 640, 360, 80, 0)
	return true
}
func runTraverse(ctx *maa.Context, startMode, finishMode string, timeoutMs int) bool {
	startDelayMs := expressDeliveryStepStartDelay
	if startMode == "key_e" {
		startDelayMs = 0
	}
	paramBytes, err := json.Marshal(traverseZiplineParams{
		StationaryNodeName: "ExpressDeliveryZiplineStationary",
		PromptNodeName:     "ExpressDeliveryZiplinePrompt",
		PollIntervalMs:     expressDeliveryStepPollMs,
		DisappearTimeout:   expressDeliveryStepStartWaitMs,
		CompletionTimeout:  timeoutMs,
		StartDelayMs:       startDelayMs,
		StartMode:          startMode,
		FinishMode:         finishMode,
		MinMovingMs:        0,
		StableGoneMs:       expressDeliveryStepStableGone,
		StableBackMs:       expressDeliveryStepStableBack,
		StablePromptMs:     expressDeliveryStepPollMs,
		ClickX:             640,
		ClickY:             360,
	})
	if err != nil {
		return false
	}
	return (&TraverseZiplineAction{}).Run(ctx, &maa.CustomActionArg{
		CustomActionParam: string(paramBytes),
	})
}

func calcFinishMode(nextStep *routeStep) string {
	if nextStep != nil && nextStep.Kind == routeStepNext {
		return finishModePrompt
	}
	return finishModeArrival
}

func calcStepTimeoutMs(prevStep, nextStep *routeStep) int {
	if prevStep != nil && prevStep.Kind == routeStepNext && (nextStep == nil || nextStep.Kind == routeStepMove || nextStep.Kind == routeStepEnd) {
		return routeZiplineLongTimeoutMs
	}
	return routeZiplineTimeoutMs
}

func finishModeFailureText(finishMode string) string {
	if finishMode == finishModePrompt {
		return "识别到下一次 E 键提示"
	}
	return "识别到滑索静止提示"
}

func displayTargetName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "目标"
	}
	return trimmed
}
