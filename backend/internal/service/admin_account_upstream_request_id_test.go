package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func newRequireUpstreamRequestIDAdminService(repo AccountRepository, required bool) *adminServiceImpl {
	svc := &adminServiceImpl{accountRepo: repo}
	ConfigureAdminServiceFromConfig(svc, &config.Config{Gateway: config.GatewayConfig{RequireUpstreamRequestIDHeader: required}})
	return svc
}

func requireUpstreamRequestIDHeaderRequired(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, "UPSTREAM_REQUEST_ID_HEADER_REQUIRED", infraerrors.Reason(err))
	require.Contains(t, err.Error(), "X-Oneapi-Request-Id")
	require.Contains(t, err.Error(), "X-Client-Request-Id")
}

func openAIAPIKeyCreateInput(extra map[string]any) *CreateAccountInput {
	return &CreateAccountInput{
		Name:                 "sv-gpt",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "sk-test", "base_url": "https://upstream.example.com"},
		SkipDefaultGroupBind: true,
		Extra:                extra,
	}
}

func TestCreateAccount_RequireUpstreamRequestIDHeader(t *testing.T) {
	strict := newRequireUpstreamRequestIDAdminService(&upstreamBillingProbeAccountRepo{}, true)

	_, err := strict.CreateAccount(context.Background(), openAIAPIKeyCreateInput(nil))
	requireUpstreamRequestIDHeaderRequired(t, err)
	_, err = strict.CreateAccount(context.Background(), openAIAPIKeyCreateInput(map[string]any{AccountExtraUpstreamRequestIDHeader: "   "}))
	requireUpstreamRequestIDHeaderRequired(t, err)

	created, err := strict.CreateAccount(context.Background(), openAIAPIKeyCreateInput(map[string]any{AccountExtraUpstreamRequestIDHeader: " X-Oneapi-Request-Id "}))
	require.NoError(t, err)
	require.Equal(t, "X-Oneapi-Request-Id", created.Extra[AccountExtraUpstreamRequestIDHeader])

	// 只约束 OpenAI API Key 账号。
	_, err = strict.CreateAccount(context.Background(), &CreateAccountInput{
		Name: "oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "token"}, SkipDefaultGroupBind: true,
	})
	require.NoError(t, err)
	_, err = strict.CreateAccount(context.Background(), &CreateAccountInput{
		Name: "claude", Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-ant"}, SkipDefaultGroupBind: true,
	})
	require.NoError(t, err)

	// 默认关闭（HK）：不配头照常创建。
	lenient := newRequireUpstreamRequestIDAdminService(&upstreamBillingProbeAccountRepo{}, false)
	_, err = lenient.CreateAccount(context.Background(), openAIAPIKeyCreateInput(nil))
	require.NoError(t, err)
}

func TestUpdateAccount_RequireUpstreamRequestIDHeader(t *testing.T) {
	legacy := &Account{
		ID: 7, Name: "legacy", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{},
	}
	repo := newUpstreamRequestIDAccountRepo(legacy)
	strict := newRequireUpstreamRequestIDAdminService(repo, true)

	// 整体替换 extra 但没带头：拒绝。
	_, err := strict.UpdateAccount(context.Background(), legacy.ID, &UpdateAccountInput{Extra: map[string]any{"note": "x"}})
	requireUpstreamRequestIDHeaderRequired(t, err)
	_, err = strict.UpdateAccount(context.Background(), legacy.ID, &UpdateAccountInput{Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: ""}})
	requireUpstreamRequestIDHeaderRequired(t, err)
	stored, _ := repo.GetByID(context.Background(), legacy.ID)
	require.NotContains(t, stored.Extra, "note", "rejected update must not be persisted")

	// 不碰 extra 的更新（改名、改状态）不因历史账号缺头失败。
	_, err = strict.UpdateAccount(context.Background(), legacy.ID, &UpdateAccountInput{Name: "renamed"})
	require.NoError(t, err)

	// 补上头即可保存。
	updated, err := strict.UpdateAccount(context.Background(), legacy.ID, &UpdateAccountInput{
		Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Client-Request-Id"},
	})
	require.NoError(t, err)
	require.Equal(t, "X-Client-Request-Id", updated.Extra[AccountExtraUpstreamRequestIDHeader])

	// 把 OAuth 账号改成 API Key 也要带头。
	oauth := &Account{
		ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"access_token": "token"}, Extra: map[string]any{},
	}
	strictOAuth := newRequireUpstreamRequestIDAdminService(newUpstreamRequestIDAccountRepo(oauth), true)
	_, err = strictOAuth.UpdateAccount(context.Background(), oauth.ID, &UpdateAccountInput{
		Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-test"},
	})
	requireUpstreamRequestIDHeaderRequired(t, err)

	// 默认关闭：照旧放行。
	lenientRepo := newUpstreamRequestIDAccountRepo(&Account{
		ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{},
	})
	_, err = newRequireUpstreamRequestIDAdminService(lenientRepo, false).UpdateAccount(context.Background(), 9, &UpdateAccountInput{Extra: map[string]any{"note": "x"}})
	require.NoError(t, err)
}

func newUpstreamRequestIDAccountRepo(account *Account) *upstreamBillingProbeAccountRepo {
	return &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
}
