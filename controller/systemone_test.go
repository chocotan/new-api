package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestSystemOneRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMemory, previousRedis, previousBatch := common.MemoryCacheEnabled, common.RedisEnabled, common.BatchUpdateEnabled
	previousConsume, previousCount, previousRetries := common.LogConsumeEnabled, constant.CountToken, common.RetryTimes
	previousRatios, previousGroups := ratio_setting.ModelRatio2JSONString(), ratio_setting.GroupRatio2JSONString()
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.MemoryCacheEnabled, common.RedisEnabled, common.BatchUpdateEnabled = previousMemory, previousRedis, previousBatch
		common.LogConsumeEnabled, constant.CountToken, common.RetryTimes = previousConsume, previousCount, previousRetries
		common.SetDatabaseTypes(previousMain, previousLog)
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousGroups))
	})
	common.MemoryCacheEnabled, common.RedisEnabled, common.BatchUpdateEnabled = false, false, false
	common.LogConsumeEnabled, constant.CountToken, common.RetryTimes = true, false, 0

	dialect := common.DatabaseTypeSQLite
	var dialector gorm.Dialector = sqlite.Open(":memory:")
	switch os.Getenv("SYSTEMONE_TEST_DIALECT") {
	case "mysql":
		require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
		dialector = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
		dialect = common.DatabaseTypeMySQL
	case "postgres":
		require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
		dialector = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
		dialect = common.DatabaseTypePostgreSQL
	case "", "sqlite":
	default:
		t.Fatal("unknown SYSTEMONE_TEST_DIALECT")
	}
	common.SetDatabaseTypes(dialect, dialect)
	database, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}, &model.UserSubscription{}))
	model.DB, model.LOG_DB = database, database

	const initial = 1000000
	const requestBody = `{"model":"alias","state":{"z":1,"a":{"y":2,"b":3}},"questions":{"z":{"type":"choice","instructions":"route","criteria":{"z":"Z","a":"A"}},"a":{"type":"noul","instructions":"yes?"},"s":{"type":"score","instructions":"rank","criteria":[{"level":"low"},{"level":"high"}]}},"provider":{"only":["fixture"]}}`
	const answers = `{"z":{"type":"choice","choice":"z","probabilities":{"z":0.8,"a":0.2},"confidence":0.8},"a":{"type":"noul","noul":0.9},"s":{"type":"score","score":0.8,"legend":{"0":{"level":"low"},"1":{"level":"high"}}}}`
	for _, tc := range []struct {
		name              string
		channelType       int
		override          string
		upstreamStatus    int
		response          string
		wantStatus, calls int
	}{
		{name: "OpenRouter mapping and usage", wantStatus: http.StatusOK, calls: 1},
		{name: "parameter override", override: `{"provider":{"require_parameters":true}}`, wantStatus: http.StatusOK, calls: 1},
		{name: "unsupported channel", channelType: constant.ChannelTypeOpenAI, wantStatus: http.StatusBadRequest},
		{name: "invalid override", override: `{"messages":[]}`, wantStatus: http.StatusBadRequest},
		{name: "upstream error forwarded", upstreamStatus: http.StatusTooManyRequests, wantStatus: http.StatusTooManyRequests, calls: 1},
		{name: "invalid typed response", response: `{"model":"vendor/decision-fixture","answers":{},"usage":{"input_tokens":1000,"output_tokens":0}}`, wantStatus: http.StatusBadGateway, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, table := range []any{&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}} {
				require.NoError(t, database.Unscoped().Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(table).Error)
			}
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"alias":37.5}`))
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
			require.NoError(t, database.Create(&model.User{Id: 7, Username: "decision-owner", Group: "default", Quota: initial}).Error)
			require.NoError(t, database.Create(&model.Token{Id: 11, UserId: 7, Key: "fixture-token", Name: "decision-token", RemainQuota: initial}).Error)

			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "Bearer fixture-provider", r.Header.Get("Authorization"))
				assert.Equal(t, "/alpha/decisions", r.URL.Path)
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.Contains(t, string(body), `"model":"vendor/decision-fixture"`)
				assert.Contains(t, string(body), `"state":{"z":1,"a":{"y":2,"b":3}}`)
				if strings.Contains(tc.override, "require_parameters") {
					assert.Contains(t, string(body), `"provider":{"require_parameters":true}`)
				} else {
					assert.Contains(t, string(body), `"provider":{"only":["fixture"]}`)
				}
				w.Header().Set("Content-Type", "application/json")
				status := tc.upstreamStatus
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				if tc.upstreamStatus != 0 {
					_, _ = io.WriteString(w, `{"error":{"message":"upstream rejected"}}`)
					return
				}
				response := tc.response
				if response == "" {
					response = `{"model":"vendor/decision-fixture","answers":` + answers + `,"usage":{"input_tokens":1000,"output_tokens":0}}`
				}
				_, _ = io.WriteString(w, response)
			}))
			defer upstream.Close()
			mapping := `{"alias":"vendor/decision-fixture"}`
			channelType := tc.channelType
			if channelType == 0 {
				channelType = constant.ChannelTypeOpenRouter
			}
			channel := model.Channel{Id: 21, Type: channelType, Name: "fixture", Key: "fixture-provider", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: "alias", Group: "default", ModelMapping: &mapping, ParamOverride: &tc.override}
			require.NoError(t, database.Create(&channel).Error)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(requestBody))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyUserId, 7)
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserQuota, initial)
			common.SetContextKey(c, constant.ContextKeyTokenId, 11)
			common.SetContextKey(c, constant.ContextKeyTokenKey, "fixture-token")
			c.Set("username", "decision-owner")
			c.Set("token_name", "decision-token")
			require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channel, "alias"))
			Relay(c, types.RelayFormatSystemOne)

			assert.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			assert.Equal(t, int32(tc.calls), calls.Load(), "at most one paid POST")
			if tc.wantStatus == http.StatusOK {
				assert.JSONEq(t, `{"model":"vendor/decision-fixture","answers":`+answers+`,"usage":{"input_tokens":1000,"output_tokens":0}}`, recorder.Body.String())
			}
			require.Eventually(t, func() bool {
				var user model.User
				var token model.Token
				return database.First(&user, 7).Error == nil && database.First(&token, 11).Error == nil &&
					user.Quota == token.RemainQuota && (tc.wantStatus == http.StatusOK && user.Quota == initial-37500 || tc.wantStatus != http.StatusOK && user.Quota == initial)
			}, 3*time.Second, 10*time.Millisecond)
		})
	}
}
