package instance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fastjson"

	"github.com/ft-t/apimonkey/pkg/common"
	"github.com/ft-t/apimonkey/pkg/executor"
	"github.com/ft-t/apimonkey/pkg/utils"
)

type DefaultInstance struct {
	ctxID     string
	cfg       *common.Config
	mut       sync.Mutex
	ctx       context.Context
	ctxCancel context.CancelFunc
	executor  Executor
	sdk       SDK
}

func NewInstance(
	ctxID string,
	executor Executor,
	sdk SDK,
) *DefaultInstance {
	return &DefaultInstance{
		ctxID:    ctxID,
		mut:      sync.Mutex{},
		executor: executor,
		sdk:      sdk,
	}
}

func (i *DefaultInstance) SDK() SDK {
	return i.sdk
}

func (i *DefaultInstance) Executor() Executor {
	return i.executor
}

func (i *DefaultInstance) ContextID() string {
	return i.ctxID
}

func (i *DefaultInstance) SetConfig(payload *fastjson.Value) error {
	settingsBytes := payload.MarshalTo(nil)
	var tempConfig common.Config

	if err := json.Unmarshal(settingsBytes, &tempConfig); err != nil {
		i.ShowAlert()
		return errors.Wrap(err, "failed to unmarshal settings")
	}

	i.cfg = &tempConfig

	return nil
}

func (i *DefaultInstance) ShowAlert() {
	i.sdk.ShowAlert(i.ctxID)
}

func (i *DefaultInstance) ShowOk() {
	i.sdk.ShowOk(i.ctxID)
}

func (i *DefaultInstance) StartAsync(immediate bool) {
	i.mut.Lock()
	defer i.mut.Unlock()

	i.stopWithoutLock()

	ctx, cancel := context.WithCancel(context.Background())
	i.ctx = ctx
	i.ctxCancel = cancel

	go i.run(immediate)
}

func (i *DefaultInstance) run(immediate bool) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error().Msgf("panic in run: %v", rec)
		}
	}()
	ctx := i.ctx

	if immediate {
		newLogger := log.With().
			Str("id", uuid.NewString()).
			Str("ctxID", i.ctxID).
			Logger()

		innerCtx, innerCancel := context.WithCancel(ctx)
		innerCtx = newLogger.WithContext(innerCtx)

		i.ExecuteSingleRequest(innerCtx)
		innerCancel()
	}

	if i.cfg == nil || i.cfg.IntervalSeconds <= 0 {
		return
	}

	ticker := time.NewTicker(time.Duration(i.cfg.IntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newLogger := log.With().
				Str("id", uuid.NewString()).
				Str("ctxID", i.ctxID).
				Logger()

			innerCtx, innerCancel := context.WithCancel(ctx)
			innerCtx = newLogger.WithContext(innerCtx)

			i.ExecuteSingleRequest(innerCtx)
			innerCancel()
		}
	}
}

func (i *DefaultInstance) ExecuteSingleRequest(
	ctx context.Context,
) {
	if i.cfg == nil {
		zerolog.Ctx(ctx).Error().Msg("config is nil, cannot execute request")
		i.ShowAlert()
		return
	}

	resp, err := i.executor.Execute(ctx, executor.ExecuteRequest{
		Config: *i.cfg,
	})
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("error executing request")
		i.ShowAlert()
		return
	}

	if handleErr := i.HandleResponse(ctx, resp); handleErr != nil {
		zerolog.Ctx(ctx).Err(handleErr).Msg("error handling response")
		i.ShowAlert()
		return
	}

	if i.cfg.ShowSuccessNotification {
		i.ShowOk()
	}
}

func (i *DefaultInstance) HandleResponse(
	ctx context.Context,
	response *executor.ExecuteResponse,
) error {
	var sb strings.Builder
	prefix, err := utils.ExecuteTemplate(i.cfg.TitlePrefix, i.cfg.TemplateParameters)
	if err != nil {
		return errors.Wrap(err, "failed to execute template on prefix")
	}

	if prefix != "" {
		sb.WriteString(strings.ReplaceAll(prefix, "\\n", "\n") + "\n")
	}

	if len(i.cfg.ResponseMapper) == 0 {
		sb.WriteString(response.Response)

		i.sdk.SetTitle(i.ctxID, sb.String(), 0)
		i.sdk.SetImage(i.ctxID, "", 0)

		return nil
	}

	def, defaultOk := i.cfg.ResponseMapper["*"]
	mapped, ok := i.cfg.ResponseMapper[response.Response]

	if !ok && defaultOk {
		mapped = def
	}

	if mapped == "" {
		return errors.New("no mapping found")
	}

	if strings.HasSuffix(mapped, ".png") || strings.HasSuffix(mapped, ".svg") {
		if sb.Len() > 0 {
			i.sdk.SetTitle(i.ctxID, sb.String(), 0)
		}

		return i.handleImageMapping(ctx, mapped)
	} else {
		sb.WriteString(mapped)
		i.sdk.SetTitle(i.ctxID, sb.String(), 0)
		i.sdk.SetImage(i.ctxID, "", 0)
	}

	return nil
}

func (i *DefaultInstance) handleImageMapping(_ context.Context, mapped string) error {
	fileData, err := utils.ReadFile(mapped)

	if err != nil {
		return errors.Join(err, errors.New("image file not found"))
	}

	imageData := ""
	if strings.HasSuffix(mapped, ".png") {
		imageData = fmt.Sprintf("data:image/png;base64, %v", base64.StdEncoding.EncodeToString(fileData))
	} else if strings.HasSuffix(mapped, ".svg") {
		imageData = fmt.Sprintf("data:image/svg+xml;charset=utf8,%v", string(fileData))
	}

	i.sdk.SetImage(i.ctxID, imageData, 0)

	return nil
}

func (i *DefaultInstance) Stop() {
	i.mut.Lock()
	defer i.mut.Unlock()

	i.stopWithoutLock()
}

func (i *DefaultInstance) stopWithoutLock() {
	if i.ctxCancel != nil {
		i.ctxCancel()
	}

	i.ctxCancel = nil
}

func (i *DefaultInstance) KeyPressed() error {
	i.mut.Lock()
	defer i.mut.Unlock()

	if i.cfg == nil {
		i.ShowAlert()
		return errors.New("instance config is nil")
	}

	targetUrl := i.cfg.BrowserUrl
	if targetUrl == "" {
		ctx := i.ctx
		if ctx == nil {
			ctx = context.Background()
		}

		newLogger := log.With().
			Str("id", uuid.NewString()).
			Str("ctxID", i.ctxID).
			Logger()

		innerCtx, innerCancel := context.WithCancel(ctx)
		defer innerCancel()
		innerCtx = newLogger.WithContext(innerCtx)

		i.ExecuteSingleRequest(innerCtx)
		return nil
	}

	targetUrl, err := utils.ExecuteTemplate(targetUrl, i.cfg.TemplateParameters)
	if err != nil {
		i.ShowAlert()
		return errors.Wrap(err, "failed to execute template")
	}

	if err = utils.OpenBrowser(targetUrl); err != nil {
		i.ShowAlert()
		return err
	}

	return nil
}
