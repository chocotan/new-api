package relay

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// SystemOneHelper relays native synchronous decisions. Transport, reservation and
// usage-based settlement remain owned by the existing relay mechanisms.
func SystemOneHelper(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	info.InitChannelMeta(c)
	adaptor := GetAdaptor(info.ApiType)
	if info.ChannelType != constant.ChannelTypeOpenRouter {
		return types.NewErrorWithStatusCode(errors.New("channel does not support System One"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	original, ok := info.Request.(*dto.SystemOneRequest)
	if !ok {
		return systemOneResponseError("invalid System One request")
	}
	raw, err := common.Marshal(original)
	if err != nil {
		return systemOneResponseError("invalid System One request")
	}
	var request dto.SystemOneRequest
	if common.Unmarshal(raw, &request) != nil {
		return systemOneResponseError("invalid System One request")
	}
	if err = helper.ModelMappedHelper(c, info, &request); err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}
	raw, err = common.Marshal(request)
	if err != nil {
		return systemOneResponseError("invalid System One request")
	}
	// Advanced operations may decode/reencode arbitrary objects. Until their
	// order semantics are defined for System One, admit only scalar/routing
	// overrides; untouched state/questions keep their original byte order.
	for key := range info.ParamOverride {
		switch key {
		case "model", "provider", "session_id", "trace", "user":
		default:
			return types.NewErrorWithStatusCode(errors.New("unsupported System One parameter override: "+key), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
	}
	if len(info.ParamOverride) > 0 {
		raw, err = relaycommon.ApplyParamOverrideWithRelayInfo(raw, info)
		if err != nil {
			return newAPIErrorFromParamOverride(err)
		}
	}
	if err = common.Unmarshal(raw, &request); err == nil {
		err = request.Validate()
	}
	if err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	info.UpstreamModelName = request.Model
	adaptor.Init(info)
	response, err := adaptor.DoRequest(c, info, bytes.NewReader(raw))
	if err != nil {
		var apiErr *types.NewAPIError
		if errors.As(err, &apiErr) {
			return apiErr
		}
		return types.NewError(err, types.ErrorCodeDoRequestFailed, types.ErrOptionWithSkipRetry())
	}
	resp, ok := response.(*http.Response)
	if !ok || resp == nil {
		return systemOneResponseError("System One response missing")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return service.RelayErrorHandler(c.Request.Context(), resp, false)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(body) > 16<<20 {
		return systemOneResponseError("invalid System One response")
	}
	usage, typedErr := request.ValidateResponse(body)
	if typedErr != nil {
		return systemOneResponseError(typedErr.Error())
	}
	service.PostTextConsumeQuota(c, info, usage, nil)
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "application/json") {
		contentType = "application/json"
	}
	c.Data(http.StatusOK, contentType, body)
	return nil
}

func systemOneResponseError(reason string) *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New(reason), types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
}
