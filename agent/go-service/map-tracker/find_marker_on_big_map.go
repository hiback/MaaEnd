// Copyright (c) 2026 Harry Huang
package maptracker

import (
	"encoding/json"
	"fmt"
	"image"
	"math"
	"regexp"
	"sync"

	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/minicv"
	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/resource"
	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

// MapTrackerFindMarkerOnBigMapParam represents the custom_recognition_param
// for MapTrackerFindMarkerOnBigMap.
type MapTrackerFindMarkerOnBigMapParam struct {
	// MapName is the target map name (e.g. "map02_lv002").
	MapName string `json:"map_name"`
	// Target is the map-space coordinate around which the marker is expected.
	Target [2]float64 `json:"target"`
	// Template is the marker image path relative to the resource image directory,
	// e.g. "ExpressDelivery/DeliveryTargetIcon.png".
	Template string `json:"template"`
	// Threshold is the minimum NCC score to consider a template match. Defaults to 0.8.
	Threshold float64 `json:"threshold,omitempty"`
	// Tolerance is the half-side length (in screen pixels) of the square ROI
	// centered on the predicted target screen position. Defaults to 16.
	Tolerance int `json:"tolerance,omitempty"`
	// GreenMask ignores pure green template pixels during matching.
	GreenMask bool `json:"green_mask,omitempty"`
}

// MapTrackerFindMarkerOnBigMapResult is the Detail payload for a successful match.
type MapTrackerFindMarkerOnBigMapResult struct {
	MapName       string     `json:"mapName"`
	Target        [2]float64 `json:"target"`
	TargetScreenX float64    `json:"targetScreenX"`
	TargetScreenY float64    `json:"targetScreenY"`
	MatchX        int        `json:"matchX"`
	MatchY        int        `json:"matchY"`
	MatchScore    float64    `json:"matchScore"`
}

// MapTrackerFindMarkerOnBigMap is a custom recognition that reports whether a
// fixed UI marker template is visible near a given map-space coordinate on the
// currently opened big map. Useful for identifying which of several known map
// locations currently carries a quest marker, without any panning side effects.
type MapTrackerFindMarkerOnBigMap struct {
	loaderMu sync.Mutex
	loaders  map[string]*minicv.TemplateLoader
}

var _ maa.CustomRecognitionRunner = &MapTrackerFindMarkerOnBigMap{}

// Run implements maa.CustomRecognitionRunner.
func (r *MapTrackerFindMarkerOnBigMap) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	param, err := r.parseParam(arg.CustomRecognitionParam)
	if err != nil {
		log.Error().Err(err).Msg("Failed to parse parameters for MapTrackerFindMarkerOnBigMap")
		return nil, false
	}

	// Reuse MapTrackerBigMapInfer on the current frame to obtain viewport mapping.
	inferConfig := map[string]any{
		"map_name_regex": "^" + regexp.QuoteMeta(param.MapName) + "$",
		"threshold":      mapTrackerBigMapInferDefaultParam.Threshold,
	}
	inferConfigBytes, err := json.Marshal(inferConfig)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal big-map inference config")
		return nil, false
	}

	taskDetail, err := ctx.GetTaskJob().GetDetail()
	if err != nil {
		log.Error().Err(err).Msg("Failed to get task detail")
		return nil, false
	}

	inferWrapper, inferHit := mapTrackerBigMapInferRunner.Run(ctx, &maa.CustomRecognitionArg{
		TaskID:                 taskDetail.ID,
		CurrentTaskName:        taskDetail.Entry,
		CustomRecognitionName:  "MapTrackerBigMapInfer",
		CustomRecognitionParam: string(inferConfigBytes),
		Img:                    arg.Img,
		Roi:                    arg.Roi,
	})
	if !inferHit || inferWrapper == nil || inferWrapper.Detail == "" {
		log.Info().Str("map", param.MapName).Msg("Big-map inference did not hit for find marker")
		return nil, false
	}

	var inferRes MapTrackerBigMapInferResult
	if err := json.Unmarshal([]byte(inferWrapper.Detail), &inferRes); err != nil {
		log.Error().Err(err).Msg("Failed to unmarshal big-map inference result")
		return nil, false
	}
	if inferRes.MapName != param.MapName {
		log.Info().Str("expect", param.MapName).Str("got", inferRes.MapName).Msg("Big-map inference returned unexpected map")
		return nil, false
	}

	targetScreenX, targetScreenY := inferRes.ViewPort.GetScreenCoordOf(param.Target[0], param.Target[1])
	if !inferRes.ViewPort.IsViewCoordInView(targetScreenX, targetScreenY) {
		log.Info().
			Str("map", param.MapName).
			Float64("targetX", param.Target[0]).
			Float64("targetY", param.Target[1]).
			Float64("targetScreenX", targetScreenX).
			Float64("targetScreenY", targetScreenY).
			Msg("Target is outside the current big-map viewport")
		return nil, false
	}

	tpl, err := r.getTemplate(param.Template)
	if err != nil {
		log.Error().Err(err).Str("template", param.Template).Msg("Failed to load marker template")
		return nil, false
	}

	screen := minicv.ImageConvertRGBA(arg.Img)
	screenW, screenH := screen.Rect.Dx(), screen.Rect.Dy()

	tw, th := tpl.Image.Rect.Dx(), tpl.Image.Rect.Dy()
	cx := int(math.Round(targetScreenX))
	cy := int(math.Round(targetScreenY))

	rectX := cx - param.Tolerance
	rectY := cy - param.Tolerance
	rectW := 2*param.Tolerance + 1
	rectH := 2*param.Tolerance + 1
	if rectX < 0 {
		rectW += rectX
		rectX = 0
	}
	if rectY < 0 {
		rectH += rectY
		rectY = 0
	}
	if rectX+rectW > screenW {
		rectW = screenW - rectX
	}
	if rectY+rectH > screenH {
		rectH = screenH - rectY
	}
	if rectW <= 0 || rectH <= 0 {
		log.Warn().Int("rectW", rectW).Int("rectH", rectH).Msg("Search area invalid after clipping")
		return nil, false
	}

	var matchX, matchY, score float64
	if param.GreenMask {
		matchX, matchY, score = matchTemplateInAreaWithGreenMask(
			screen,
			tpl.Image,
			[4]int{rectX, rectY, rectW, rectH},
		)
	} else {
		matchX, matchY, score = minicv.MatchTemplateInArea(
			screen,
			minicv.GetIntegralArray(screen),
			tpl.Image,
			tpl.Stats,
			[4]int{rectX, rectY, rectW, rectH},
		)
	}
	if score < param.Threshold {
		log.Info().
			Str("map", param.MapName).
			Float64("targetX", param.Target[0]).
			Float64("targetY", param.Target[1]).
			Float64("score", score).
			Float64("threshold", param.Threshold).
			Msg("Marker template not matched near target")
		return nil, false
	}

	result := MapTrackerFindMarkerOnBigMapResult{
		MapName:       param.MapName,
		Target:        param.Target,
		TargetScreenX: targetScreenX,
		TargetScreenY: targetScreenY,
		MatchX:        int(math.Round(matchX)),
		MatchY:        int(math.Round(matchY)),
		MatchScore:    score,
	}

	detailJSON, err := json.Marshal(result)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal find-marker result")
		return nil, false
	}

	log.Info().
		Str("map", param.MapName).
		Float64("targetX", param.Target[0]).
		Float64("targetY", param.Target[1]).
		Float64("score", score).
		Msg("Marker found on big map near target")

	return &maa.CustomRecognitionResult{
		Box:    maa.Rect{result.MatchX, result.MatchY, tw, th},
		Detail: string(detailJSON),
	}, true
}

func (r *MapTrackerFindMarkerOnBigMap) parseParam(paramStr string) (*MapTrackerFindMarkerOnBigMapParam, error) {
	if paramStr == "" {
		return nil, fmt.Errorf("custom_recognition_param is required")
	}
	var param MapTrackerFindMarkerOnBigMapParam
	if err := json.Unmarshal([]byte(paramStr), &param); err != nil {
		return nil, fmt.Errorf("failed to unmarshal parameters: %w", err)
	}
	if param.MapName == "" {
		return nil, fmt.Errorf("map_name must be provided")
	}
	if param.Template == "" {
		return nil, fmt.Errorf("template must be provided")
	}
	if math.IsNaN(param.Target[0]) || math.IsInf(param.Target[0], 0) ||
		math.IsNaN(param.Target[1]) || math.IsInf(param.Target[1], 0) {
		return nil, fmt.Errorf("target must contain finite numbers")
	}
	if param.Threshold == 0.0 {
		param.Threshold = 0.8
	}
	if param.Threshold < 0.0 || param.Threshold > 1.0 {
		return nil, fmt.Errorf("threshold must be in (0, 1]")
	}
	if param.Tolerance <= 0 {
		param.Tolerance = 16
	}
	return &param, nil
}

func (r *MapTrackerFindMarkerOnBigMap) getTemplate(relPath string) (*minicv.Template, error) {
	r.loaderMu.Lock()
	if r.loaders == nil {
		r.loaders = make(map[string]*minicv.TemplateLoader)
	}
	loader, ok := r.loaders[relPath]
	if !ok {
		loader = minicv.NewTemplateLoaderOfDynamicPath(func() string {
			return resource.FindResource("resource/image/" + relPath)
		})
		r.loaders[relPath] = loader
	}
	r.loaderMu.Unlock()
	return loader.Get()
}

func matchTemplateInAreaWithGreenMask(img, tpl *image.RGBA, rect [4]int) (x, y, val float64) {
	ax, ay, aw, ah := rect[0], rect[1], rect[2], rect[3]
	iw, ih := img.Rect.Dx(), img.Rect.Dy()
	tw, th := tpl.Rect.Dx(), tpl.Rect.Dy()

	minX, minY := max(0, ax-tw/2), max(0, ay-th/2)
	maxX, maxY := min(iw-tw, ax+aw-tw/2), min(ih-th, ay+ah-th/2)
	if minX > maxX || minY > maxY {
		return 0, 0, 0.0
	}

	bestX, bestY, bestScore := minX, minY, -1.0
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			score := computeGreenMaskedNCC(img, tpl, x, y)
			if score > bestScore {
				bestX, bestY, bestScore = x, y, score
			}
		}
	}

	return float64(bestX), float64(bestY), bestScore
}

func computeGreenMaskedNCC(img, tpl *image.RGBA, ox, oy int) float64 {
	iw, ih := img.Rect.Dx(), img.Rect.Dy()
	tw, th := tpl.Rect.Dx(), tpl.Rect.Dy()
	if ox < 0 || oy < 0 || ox+tw > iw || oy+th > ih {
		return 0.0
	}

	ipx, is := img.Pix, img.Stride
	tpx, ts := tpl.Pix, tpl.Stride

	var tplSum, imgSum, dot float64
	var tplSumSq, imgSumSq float64
	count := 0.0

	for y := range th {
		iOff := (oy+y)*is + ox*4
		tOff := y * ts
		for range tw {
			tr, tg, tb := tpx[tOff], tpx[tOff+1], tpx[tOff+2]
			if !(tr == 0 && tg == 255 && tb == 0) {
				ir, ig, ib := ipx[iOff], ipx[iOff+1], ipx[iOff+2]
				tplSum += float64(tr) + float64(tg) + float64(tb)
				imgSum += float64(ir) + float64(ig) + float64(ib)
				tplSumSq += float64(tr)*float64(tr) + float64(tg)*float64(tg) + float64(tb)*float64(tb)
				imgSumSq += float64(ir)*float64(ir) + float64(ig)*float64(ig) + float64(ib)*float64(ib)
				dot += float64(ir)*float64(tr) + float64(ig)*float64(tg) + float64(ib)*float64(tb)
				count += 3
			}
			iOff += 4
			tOff += 4
		}
	}

	if count <= 0 {
		return 0.0
	}

	tplMean := tplSum / count
	imgMean := imgSum / count
	tplVar := tplSumSq - count*(tplMean*tplMean)
	imgVar := imgSumSq - count*(imgMean*imgMean)
	if tplVar < 1e-12 || imgVar < 1e-12 {
		return 0.0
	}

	return (dot - count*imgMean*tplMean) / math.Sqrt(imgVar*tplVar)
}
