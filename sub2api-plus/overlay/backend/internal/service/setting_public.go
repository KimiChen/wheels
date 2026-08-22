package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

func normalizeLoginAgreementMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "checkbox":
		return "checkbox"
	default:
		return defaultLoginAgreementMode
	}
}

func defaultLoginAgreementDocuments() []LoginAgreementDocument {
	return []LoginAgreementDocument{
		{
			ID:        "terms",
			Title:     "服务条款",
			ContentMD: "",
		},
		{
			ID:        "usage-policy",
			Title:     "使用政策",
			ContentMD: "",
		},
		{
			ID:        "supported-regions",
			Title:     "支持的国家和地区",
			ContentMD: "",
		},
		{
			ID:        "service-specific-terms",
			Title:     "服务特定条款",
			ContentMD: "",
		},
	}
}

func normalizeLoginAgreementDocumentID(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	lastSeparator := false
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			_, _ = b.WriteRune(r)
			lastSeparator = false
			continue
		}
		if r == '-' || r == '_' || r == ' ' || r == '.' || r == '/' {
			if !lastSeparator && b.Len() > 0 {
				if r == '_' {
					_, _ = b.WriteRune('_')
				} else {
					_, _ = b.WriteRune('-')
				}
				lastSeparator = true
			}
		}
	}
	return strings.Trim(b.String(), "-_")
}

func normalizeLoginAgreementDocuments(docs []LoginAgreementDocument) []LoginAgreementDocument {
	normalized := make([]LoginAgreementDocument, 0, len(docs))
	seen := make(map[string]int, len(docs))
	for i, doc := range docs {
		title := strings.TrimSpace(doc.Title)
		content := strings.TrimSpace(doc.ContentMD)
		if title == "" && content == "" {
			continue
		}
		id := normalizeLoginAgreementDocumentID(doc.ID)
		if id == "" {
			sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s", i, title, content)))
			id = hex.EncodeToString(sum[:])[:12]
		}
		baseID := id
		for suffix := 2; seen[id] > 0; suffix++ {
			id = fmt.Sprintf("%s-%d", baseID, suffix)
		}
		seen[id]++
		normalized = append(normalized, LoginAgreementDocument{
			ID:        id,
			Title:     title,
			ContentMD: content,
		})
	}
	return normalized
}

func parseLoginAgreementDocuments(raw string) []LoginAgreementDocument {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultLoginAgreementDocuments()
	}
	var docs []LoginAgreementDocument
	if err := json.Unmarshal([]byte(raw), &docs); err != nil {
		return defaultLoginAgreementDocuments()
	}
	docs = normalizeLoginAgreementDocuments(docs)
	if len(docs) == 0 {
		return defaultLoginAgreementDocuments()
	}
	return docs
}

func marshalLoginAgreementDocuments(docs []LoginAgreementDocument) (string, error) {
	normalized := normalizeLoginAgreementDocuments(docs)
	if len(normalized) == 0 {
		normalized = defaultLoginAgreementDocuments()
	}
	b, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal login agreement documents: %w", err)
	}
	return string(b), nil
}

func buildLoginAgreementRevision(updatedAt string, docs []LoginAgreementDocument) string {
	normalized := normalizeLoginAgreementDocuments(docs)
	payload, err := json.Marshal(struct {
		UpdatedAt string                   `json:"updated_at"`
		Documents []LoginAgreementDocument `json:"documents"`
	}{
		UpdatedAt: strings.TrimSpace(updatedAt),
		Documents: normalized,
	})
	if err != nil {
		payload = []byte(strings.TrimSpace(updatedAt))
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:16]
}

// GetFrontendURL 获取前端基础URL（数据库优先，fallback 到配置文件）
func (s *SettingService) GetFrontendURL(ctx context.Context) string {
	val, err := s.settingRepo.GetValue(ctx, SettingKeyFrontendURL)
	if err == nil && strings.TrimSpace(val) != "" {
		return strings.TrimSpace(val)
	}
	return s.cfg.Server.FrontendURL
}

// GetPublicSettings 获取公开设置（无需登录）
func (s *SettingService) GetPublicSettings(ctx context.Context) (*PublicSettings, error) {
	keys := []string{
		SettingKeyRegistrationEnabled,
		SettingKeyEmailVerifyEnabled,
		SettingKeyForceEmailOnThirdPartySignup,
		SettingKeyRegistrationEmailSuffixWhitelist,
		SettingKeyRegistrationEmailDomainQuotaEnabled,
		SettingKeyPromoCodeEnabled,
		SettingKeyPasswordResetEnabled,
		SettingKeyInvitationCodeEnabled,
		SettingKeyTotpEnabled,
		SettingKeyPasskeyEnabled,
		SettingKeyLoginAgreementEnabled,
		SettingKeyLoginAgreementMode,
		SettingKeyLoginAgreementUpdatedAt,
		SettingKeyLoginAgreementDocuments,
		SettingKeyTurnstileEnabled,
		SettingKeyTurnstileSiteKey,
		SettingKeyTencentCaptchaEnabled,
		SettingKeyTencentCaptchaAppID,
		SettingKeyTencentCaptchaRegion,
		SettingKeyAliyunCaptchaEnabled,
		SettingKeyAliyunCaptchaSceneID,
		SettingKeyAliyunCaptchaPrefix,
		SettingKeyAliyunCaptchaRegion,
		SettingKeyAPIKeyACLTrustForwardedIP,
		SettingKeySiteName,
		SettingKeySiteLogo,
		SettingKeySiteSubtitle,
		SettingKeyAPIBaseURL,
		SettingKeyContactInfo,
		SettingKeyDocURL,
		SettingKeyHomeContent,
		SettingKeyCompactHomeEnabled,
		SettingKeyHideCcsImportButton,
		SettingKeyPurchaseSubscriptionEnabled,
		SettingKeyPurchaseSubscriptionURL,
		SettingKeyTableDefaultPageSize,
		SettingKeyTablePageSizeOptions,
		SettingKeyCustomMenuItems,
		SettingKeyCustomEndpoints,
		SettingKeyLinuxDoConnectEnabled,
		SettingKeyDingTalkConnectEnabled,
		SettingKeyWeChatConnectEnabled,
		SettingKeyWeChatConnectAppID,
		SettingKeyWeChatConnectAppSecret,
		SettingKeyWeChatConnectOpenAppID,
		SettingKeyWeChatConnectOpenAppSecret,
		SettingKeyWeChatConnectMPAppID,
		SettingKeyWeChatConnectMPAppSecret,
		SettingKeyWeChatConnectMobileAppID,
		SettingKeyWeChatConnectMobileAppSecret,
		SettingKeyWeChatConnectOpenEnabled,
		SettingKeyWeChatConnectMPEnabled,
		SettingKeyWeChatConnectMobileEnabled,
		SettingKeyWeChatConnectMode,
		SettingKeyWeChatConnectScopes,
		SettingKeyWeChatConnectRedirectURL,
		SettingKeyWeChatConnectFrontendRedirectURL,
		SettingKeyBackendModeEnabled,
		SettingPaymentEnabled,
		SettingKeyOIDCConnectEnabled,
		SettingKeyOIDCConnectProviderName,
		SettingKeyGitHubOAuthEnabled,
		SettingKeyGitHubOAuthClientID,
		SettingKeyGitHubOAuthClientSecret,
		SettingKeyGoogleOAuthEnabled,
		SettingKeyGoogleOAuthClientID,
		SettingKeyGoogleOAuthClientSecret,
		SettingKeyBalanceLowNotifyEnabled,
		SettingKeyBalanceLowNotifyThreshold,
		SettingKeyBalanceLowNotifyRechargeURL,
		SettingKeyAccountQuotaNotifyEnabled,
		SettingKeyChannelMonitorEnabled,
		SettingKeyChannelMonitorMode,
		SettingKeyChannelMonitorDefaultIntervalSeconds,
		SettingKeyChannelMonitorHideThroughput,
		SettingKeyChannelMonitorShowQuota,
		SettingKeyAvailableChannelsEnabled,
		SettingKeyModelPlazaEnabled,
		SettingKeyModelPlazaRequireAuth,
		SettingKeyAffiliateEnabled,
		SettingKeyRiskControlEnabled,
		SettingKeyAllowUserViewErrorRequests,
	}

	settings, err := s.settingRepo.GetMultiple(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("get public settings: %w", err)
	}

	linuxDoEnabled := false
	if raw, ok := settings[SettingKeyLinuxDoConnectEnabled]; ok {
		linuxDoEnabled = raw == "true"
	} else {
		linuxDoEnabled = s.cfg != nil && s.cfg.LinuxDo.Enabled
	}
	dingTalkEnabled := false
	if raw, ok := settings[SettingKeyDingTalkConnectEnabled]; ok {
		dingTalkEnabled = raw == "true"
	} else {
		dingTalkEnabled = s.cfg != nil && s.cfg.DingTalk.Enabled
	}
	oidcEnabled := false
	if raw, ok := settings[SettingKeyOIDCConnectEnabled]; ok {
		oidcEnabled = raw == "true"
	} else {
		oidcEnabled = s.cfg != nil && s.cfg.OIDC.Enabled
	}
	oidcProviderName := strings.TrimSpace(settings[SettingKeyOIDCConnectProviderName])
	if oidcProviderName == "" && s.cfg != nil {
		oidcProviderName = strings.TrimSpace(s.cfg.OIDC.ProviderName)
	}
	if oidcProviderName == "" {
		oidcProviderName = "OIDC"
	}
	gitHubEnabled := s.emailOAuthPublicEnabled(settings, "github")
	googleEnabled := s.emailOAuthPublicEnabled(settings, "google")
	weChatEnabled, weChatOpenEnabled, weChatMPEnabled, weChatMobileEnabled := s.weChatOAuthCapabilitiesFromSettings(settings)

	// Password reset requires email verification to be enabled
	emailVerifyEnabled := settings[SettingKeyEmailVerifyEnabled] == "true"
	passwordResetEnabled := emailVerifyEnabled && settings[SettingKeyPasswordResetEnabled] == "true"
	registrationEmailSuffixWhitelist := ParseRegistrationEmailSuffixWhitelist(
		settings[SettingKeyRegistrationEmailSuffixWhitelist],
	)
	tableDefaultPageSize, tablePageSizeOptions := parseTablePreferences(
		settings[SettingKeyTableDefaultPageSize],
		settings[SettingKeyTablePageSizeOptions],
	)
	loginAgreementDocuments := parseLoginAgreementDocuments(settings[SettingKeyLoginAgreementDocuments])
	loginAgreementUpdatedAt := strings.TrimSpace(settings[SettingKeyLoginAgreementUpdatedAt])
	if loginAgreementUpdatedAt == "" {
		loginAgreementUpdatedAt = defaultLoginAgreementDate
	}

	var balanceLowNotifyThreshold float64
	if v, err := strconv.ParseFloat(settings[SettingKeyBalanceLowNotifyThreshold], 64); err == nil && v >= 0 {
		balanceLowNotifyThreshold = v
	}

	return &PublicSettings{
		RegistrationEnabled:                 settings[SettingKeyRegistrationEnabled] == "true",
		EmailVerifyEnabled:                  emailVerifyEnabled,
		ForceEmailOnThirdPartySignup:        settings[SettingKeyForceEmailOnThirdPartySignup] == "true",
		RegistrationEmailSuffixWhitelist:    registrationEmailSuffixWhitelist,
		RegistrationEmailDomainQuotaEnabled: settings[SettingKeyRegistrationEmailDomainQuotaEnabled] == "true",
		PromoCodeEnabled:                    settings[SettingKeyPromoCodeEnabled] != "false", // 默认启用
		PasswordResetEnabled:                passwordResetEnabled,
		InvitationCodeEnabled:               settings[SettingKeyInvitationCodeEnabled] == "true",
		TotpEnabled:                         settings[SettingKeyTotpEnabled] == "true",
		PasskeyEnabled:                      s.passkeyConfigured() && s.passkeySettingEnabled(settings),
		LoginAgreementEnabled:               settings[SettingKeyLoginAgreementEnabled] == "true" && len(loginAgreementDocuments) > 0,
		LoginAgreementMode:                  normalizeLoginAgreementMode(settings[SettingKeyLoginAgreementMode]),
		LoginAgreementUpdatedAt:             loginAgreementUpdatedAt,
		LoginAgreementRevision:              buildLoginAgreementRevision(loginAgreementUpdatedAt, loginAgreementDocuments),
		LoginAgreementDocuments:             loginAgreementDocuments,
		TurnstileEnabled:                    settings[SettingKeyTurnstileEnabled] == "true",
		TurnstileSiteKey:                    settings[SettingKeyTurnstileSiteKey],
		TencentCaptchaEnabled:               settings[SettingKeyTencentCaptchaEnabled] == "true",
		TencentCaptchaAppID:                 settings[SettingKeyTencentCaptchaAppID],
		TencentCaptchaRegion:                normalizeTencentCaptchaRegion(settings[SettingKeyTencentCaptchaRegion]),
		AliyunCaptchaEnabled:                settings[SettingKeyAliyunCaptchaEnabled] == "true",
		AliyunCaptchaSceneID:                settings[SettingKeyAliyunCaptchaSceneID],
		AliyunCaptchaPrefix:                 settings[SettingKeyAliyunCaptchaPrefix],
		AliyunCaptchaRegion:                 normalizeAliyunCaptchaRegion(settings[SettingKeyAliyunCaptchaRegion]),
		SiteName:                            s.getStringOrDefault(settings, SettingKeySiteName, "Sub2API"),
		SiteLogo:                            settings[SettingKeySiteLogo],
		SiteSubtitle:                        s.getStringOrDefault(settings, SettingKeySiteSubtitle, "Subscription to API Conversion Platform"),
		APIBaseURL:                          settings[SettingKeyAPIBaseURL],
		ContactInfo:                         settings[SettingKeyContactInfo],
		DocURL:                              settings[SettingKeyDocURL],
		HomeContent:                         settings[SettingKeyHomeContent],
		CompactHomeEnabled:                  settings[SettingKeyCompactHomeEnabled] == "true",
		HideCcsImportButton:                 settings[SettingKeyHideCcsImportButton] == "true",
		PurchaseSubscriptionEnabled:         settings[SettingKeyPurchaseSubscriptionEnabled] == "true",
		PurchaseSubscriptionURL:             strings.TrimSpace(settings[SettingKeyPurchaseSubscriptionURL]),
		TableDefaultPageSize:                tableDefaultPageSize,
		TablePageSizeOptions:                tablePageSizeOptions,
		CustomMenuItems:                     settings[SettingKeyCustomMenuItems],
		CustomEndpoints:                     settings[SettingKeyCustomEndpoints],
		LinuxDoOAuthEnabled:                 linuxDoEnabled,
		DingTalkOAuthEnabled:                dingTalkEnabled,
		WeChatOAuthEnabled:                  weChatEnabled,
		WeChatOAuthOpenEnabled:              weChatOpenEnabled,
		WeChatOAuthMPEnabled:                weChatMPEnabled,
		WeChatOAuthMobileEnabled:            weChatMobileEnabled,
		BackendModeEnabled:                  settings[SettingKeyBackendModeEnabled] == "true",
		PaymentEnabled:                      settings[SettingPaymentEnabled] == "true",
		OIDCOAuthEnabled:                    oidcEnabled,
		OIDCOAuthProviderName:               oidcProviderName,
		GitHubOAuthEnabled:                  gitHubEnabled,
		GoogleOAuthEnabled:                  googleEnabled,
		BalanceLowNotifyEnabled:             settings[SettingKeyBalanceLowNotifyEnabled] == "true",
		AccountQuotaNotifyEnabled:           settings[SettingKeyAccountQuotaNotifyEnabled] == "true",
		BalanceLowNotifyThreshold:           balanceLowNotifyThreshold,
		BalanceLowNotifyRechargeURL:         settings[SettingKeyBalanceLowNotifyRechargeURL],

		ChannelMonitorEnabled:                !isFalseSettingValue(settings[SettingKeyChannelMonitorEnabled]),
		ChannelMonitorMode:                   normalizeChannelMonitorMode(settings[SettingKeyChannelMonitorMode]),
		ChannelMonitorDefaultIntervalSeconds: parseChannelMonitorInterval(settings[SettingKeyChannelMonitorDefaultIntervalSeconds]),
		ChannelMonitorHideThroughput:         !isFalseSettingValue(settings[SettingKeyChannelMonitorHideThroughput]),
		ChannelMonitorShowQuota:              settings[SettingKeyChannelMonitorShowQuota] == "true",

		AvailableChannelsEnabled: settings[SettingKeyAvailableChannelsEnabled] == "true",

		ModelPlazaEnabled:     settings[SettingKeyModelPlazaEnabled] == "true",
		ModelPlazaRequireAuth: settings[SettingKeyModelPlazaRequireAuth] == "true",

		AffiliateEnabled: settings[SettingKeyAffiliateEnabled] == "true",

		RiskControlEnabled: settings[SettingKeyRiskControlEnabled] == "true",

		AllowUserViewErrorRequests: settings[SettingKeyAllowUserViewErrorRequests] == "true",
	}, nil
}

// channelMonitorIntervalMin / channelMonitorIntervalMax bound the default interval
// (mirrors the monitor-level constraint but lives here so setting_service stays decoupled).
const (
	channelMonitorIntervalMin      = 15
	channelMonitorIntervalMax      = 3600
	channelMonitorIntervalFallback = 60
	defaultChannelMonitorMode      = ChannelMonitorModeV1
)

// normalizeChannelMonitorMode accepts only v1/v2; empty/invalid → v1 (safe default).
func normalizeChannelMonitorMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ChannelMonitorModeV1, "":
		return ChannelMonitorModeV1
	case ChannelMonitorModeV2:
		return ChannelMonitorModeV2
	default:
		return defaultChannelMonitorMode
	}
}

// parseChannelMonitorInterval parses the stored string and clamps to [15, 3600].
// Empty / invalid input falls back to channelMonitorIntervalFallback.
func parseChannelMonitorInterval(raw string) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return channelMonitorIntervalFallback
	}
	return clampChannelMonitorInterval(v)
}

// clampChannelMonitorInterval clamps v to the allowed range. 0 means "not provided".
func clampChannelMonitorInterval(v int) int {
	if v <= 0 {
		return 0
	}
	if v < channelMonitorIntervalMin {
		return channelMonitorIntervalMin
	}
	if v > channelMonitorIntervalMax {
		return channelMonitorIntervalMax
	}
	return v
}

// ChannelMonitorRuntime is the lightweight view of the channel monitor feature
// consumed by the runner, V2 aggregator, and user-facing handlers.
type ChannelMonitorRuntime struct {
	Enabled                bool
	Mode                   string // ChannelMonitorModeV1 or ChannelMonitorModeV2
	DefaultIntervalSeconds int
	// HideThroughput: when true, user-facing V2 APIs omit RPM/TPM scale signals.
	HideThroughput bool
	// ShowQuota: when true, user-facing monitor views keep the quota/balance
	// snapshots; otherwise the user handler strips them server-side.
	// Parsed fail-closed (only literal "true" enables). Admin always sees them.
	ShowQuota bool
}

// ActiveProbesAllowed reports whether V1 active provider probes may run.
func (r ChannelMonitorRuntime) ActiveProbesAllowed() bool {
	return r.Enabled && r.Mode == ChannelMonitorModeV1
}

// PassiveAggregationAllowed reports whether V2 passive aggregation may run.
func (r ChannelMonitorRuntime) PassiveAggregationAllowed() bool {
	return r.Enabled && r.Mode == ChannelMonitorModeV2
}

// GetChannelMonitorRuntime reads the channel monitor feature flags directly from
// the settings store. Fail-open: on error returns Enabled=true, Mode=v1, default interval.
func (s *SettingService) GetChannelMonitorRuntime(ctx context.Context) ChannelMonitorRuntime {
	if s == nil || s.settingRepo == nil {
		return ChannelMonitorRuntime{
			Enabled:                true,
			Mode:                   defaultChannelMonitorMode,
			DefaultIntervalSeconds: channelMonitorIntervalFallback,
			HideThroughput:         true,
		}
	}
	vals, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeyChannelMonitorEnabled,
		SettingKeyChannelMonitorMode,
		SettingKeyChannelMonitorDefaultIntervalSeconds,
		SettingKeyChannelMonitorHideThroughput,
		SettingKeyChannelMonitorShowQuota,
	})
	if err != nil {
		return ChannelMonitorRuntime{
			Enabled:                true,
			Mode:                   defaultChannelMonitorMode,
			DefaultIntervalSeconds: channelMonitorIntervalFallback,
			HideThroughput:         true,
		}
	}
	return ChannelMonitorRuntime{
		Enabled:                !isFalseSettingValue(vals[SettingKeyChannelMonitorEnabled]),
		Mode:                   normalizeChannelMonitorMode(vals[SettingKeyChannelMonitorMode]),
		DefaultIntervalSeconds: parseChannelMonitorInterval(vals[SettingKeyChannelMonitorDefaultIntervalSeconds]),
		HideThroughput:         !isFalseSettingValue(vals[SettingKeyChannelMonitorHideThroughput]),
		ShowQuota:              vals[SettingKeyChannelMonitorShowQuota] == "true",
	}
}

// AvailableChannelsRuntime is the lightweight view of the available-channels feature
// switch consumed by the user-facing handler.
type AvailableChannelsRuntime struct {
	Enabled bool
}

// GetAvailableChannelsRuntime reads the available-channels feature switch directly
// from the settings store. Fail-closed: on error returns Enabled=false, matching
// the opt-in default (unknown ↔ disabled).
func (s *SettingService) GetAvailableChannelsRuntime(ctx context.Context) AvailableChannelsRuntime {
	vals, err := s.settingRepo.GetMultiple(ctx, []string{SettingKeyAvailableChannelsEnabled})
	if err != nil {
		return AvailableChannelsRuntime{Enabled: false}
	}
	return AvailableChannelsRuntime{
		Enabled: vals[SettingKeyAvailableChannelsEnabled] == "true",
	}
}

// ModelPlazaRuntime is the lightweight view of the model-plaza feature consumed
// by the public plaza handler.
type ModelPlazaRuntime struct {
	Enabled     bool
	RequireAuth bool
	Description string
}

// GetModelPlazaRuntime reads the model-plaza feature switches directly from the
// settings store. Fail-closed: on error returns Enabled=false, matching the
// opt-in default (unknown ↔ disabled).
func (s *SettingService) GetModelPlazaRuntime(ctx context.Context) ModelPlazaRuntime {
	vals, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeyModelPlazaEnabled,
		SettingKeyModelPlazaRequireAuth,
		SettingKeyModelPlazaDescription,
	})
	if err != nil {
		return ModelPlazaRuntime{Enabled: false}
	}
	return ModelPlazaRuntime{
		Enabled:     vals[SettingKeyModelPlazaEnabled] == "true",
		RequireAuth: vals[SettingKeyModelPlazaRequireAuth] == "true",
		Description: vals[SettingKeyModelPlazaDescription],
	}
}

// IsUserErrorViewAllowed reads the user-facing error-requests visibility switch
// directly from the settings store. Fail-closed: on error returns false (opt-in default).
func (s *SettingService) IsUserErrorViewAllowed(ctx context.Context) bool {
	vals, err := s.settingRepo.GetMultiple(ctx, []string{SettingKeyAllowUserViewErrorRequests})
	if err != nil {
		slog.Warn("failed to get allow_user_view_error_requests setting, defaulting to false", "error", err)
		return false
	}
	return vals[SettingKeyAllowUserViewErrorRequests] == "true"
}

// PublicSettingsInjectionPayload is the sparse JSON shape embedded into HTML as
// `window.__STATIC_APP__`. Disabled booleans, empty values, and public-API-only
// settings are intentionally omitted so page source does not advertise internal
// feature names. Frontend code must treat missing flags as false.
type PublicSettingsInjectionPayload map[string]any

// GetPublicSettingsForInjection returns public settings in a format suitable for HTML injection.
// This implements the web.PublicSettingsProvider interface.
func (s *SettingService) GetPublicSettingsForInjection(ctx context.Context) (any, error) {
	settings, err := s.GetPublicSettings(ctx)
	if err != nil {
		return nil, err
	}

	payload := PublicSettingsInjectionPayload{}
	addStringSettingUnless(payload, "site_name", settings.SiteName, legacyDefaultSiteName())
	addStringSetting(payload, "site_logo", settings.SiteLogo)
	addStringSettingUnless(payload, "site_subtitle", settings.SiteSubtitle, legacyDefaultSiteSubtitle())
	addStringSetting(payload, "contact_info", settings.ContactInfo)
	addStringSetting(payload, "doc_url", settings.DocURL)
	addStringSetting(payload, "home_content", settings.HomeContent)
	addStringSetting(payload, "server_timezone", timezone.Name())
	addStringSetting(payload, "server_utc_offset", timezone.UTCOffset())

	addTrueSetting(payload, "registration_enabled", settings.RegistrationEnabled)
	if settings.RegistrationEnabled {
		addTrueSetting(payload, "email_verify_enabled", settings.EmailVerifyEnabled)
		addNonEmptyStringSliceSetting(payload, "registration_email_suffix_whitelist", settings.RegistrationEmailSuffixWhitelist)
		addTrueSetting(payload, "registration_email_domain_quota_enabled", settings.RegistrationEmailDomainQuotaEnabled)
		addTrueSetting(payload, "promo_code_enabled", settings.PromoCodeEnabled)
		addTrueSetting(payload, "invitation_code_enabled", settings.InvitationCodeEnabled)
	}
	addTrueSetting(payload, "password_reset_enabled", settings.PasswordResetEnabled)
	addTrueSetting(payload, "passkey_enabled", settings.PasskeyEnabled)
	if settings.LoginAgreementEnabled && len(settings.LoginAgreementDocuments) > 0 {
		payload["login_agreement_enabled"] = true
		addStringSetting(payload, "login_agreement_mode", settings.LoginAgreementMode)
		addStringSetting(payload, "login_agreement_updated_at", settings.LoginAgreementUpdatedAt)
		addStringSetting(payload, "login_agreement_revision", settings.LoginAgreementRevision)
		payload["login_agreement_documents"] = settings.LoginAgreementDocuments
	}
	if settings.TurnstileEnabled {
		payload["turnstile_enabled"] = true
		addStringSetting(payload, "turnstile_site_key", settings.TurnstileSiteKey)
	}
	if settings.TencentCaptchaEnabled {
		payload["tencent_captcha_enabled"] = true
		addStringSetting(payload, "tencent_captcha_app_id", settings.TencentCaptchaAppID)
		addStringSetting(payload, "tencent_captcha_region", settings.TencentCaptchaRegion)
	}
	if settings.AliyunCaptchaEnabled {
		payload["aliyun_captcha_enabled"] = true
		addStringSetting(payload, "aliyun_captcha_scene_id", settings.AliyunCaptchaSceneID)
		addStringSetting(payload, "aliyun_captcha_prefix", settings.AliyunCaptchaPrefix)
		addStringSetting(payload, "aliyun_captcha_region", settings.AliyunCaptchaRegion)
	}

	addTrueSetting(payload, "backend_mode_enabled", settings.BackendModeEnabled)
	addTrueSetting(payload, "payment_enabled", settings.PaymentEnabled)
	addTrueSetting(payload, "compact_home_enabled", settings.CompactHomeEnabled)
	if settings.ChannelMonitorEnabled {
		payload["channel_monitor_enabled"] = true
		addStringSettingUnless(payload, "channel_monitor_mode", settings.ChannelMonitorMode, defaultChannelMonitorMode)
		addIntSettingUnless(payload, "channel_monitor_default_interval_seconds", settings.ChannelMonitorDefaultIntervalSeconds, channelMonitorIntervalFallback)
		addTrueSetting(payload, "channel_monitor_hide_throughput", settings.ChannelMonitorHideThroughput)
		addTrueSetting(payload, "channel_monitor_show_quota", settings.ChannelMonitorShowQuota)
	}
	addTrueSetting(payload, "available_channels_enabled", settings.AvailableChannelsEnabled)
	if settings.ModelPlazaEnabled {
		payload["model_plaza_enabled"] = true
		addTrueSetting(payload, "model_plaza_require_auth", settings.ModelPlazaRequireAuth)
	}
	addTrueSetting(payload, "affiliate_enabled", settings.AffiliateEnabled)
	addTrueSetting(payload, "risk_control_enabled", settings.RiskControlEnabled)
	addTrueSetting(payload, "allow_user_view_error_requests", settings.AllowUserViewErrorRequests)

	return payload, nil
}

func addTrueSetting(payload PublicSettingsInjectionPayload, key string, value bool) {
	if value {
		payload[key] = true
	}
}

func addStringSetting(payload PublicSettingsInjectionPayload, key, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		payload[key] = value
	}
}

func addStringSettingUnless(payload PublicSettingsInjectionPayload, key, value string, omittedValues ...string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	for _, omitted := range omittedValues {
		if value == omitted {
			return
		}
	}
	payload[key] = value
}

func addNonEmptyStringSliceSetting(payload PublicSettingsInjectionPayload, key string, values []string) {
	if len(values) > 0 {
		payload[key] = values
	}
}

func addIntSettingUnless(payload PublicSettingsInjectionPayload, key string, value int, omittedValues ...int) {
	for _, omitted := range omittedValues {
		if value == omitted {
			return
		}
	}
	if value != 0 {
		payload[key] = value
	}
}

func legacyDefaultSiteName() string {
	return string([]byte{83, 117, 98, 50, 65, 80, 73})
}

func legacyDefaultSiteSubtitle() string {
	return string([]byte{
		83, 117, 98, 115, 99, 114, 105, 112, 116, 105, 111, 110, 32, 116, 111,
		32, 65, 80, 73, 32, 67, 111, 110, 118, 101, 114, 115, 105, 111, 110,
		32, 80, 108, 97, 116, 102, 111, 114, 109,
	})
}

// filterUserVisibleMenuItems filters out admin-only menu items from a raw JSON
// array string, returning only items with visibility != "admin".
func filterUserVisibleMenuItems(raw string) json.RawMessage {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return json.RawMessage("[]")
	}
	var items []struct {
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return json.RawMessage("[]")
	}

	// Parse full items to preserve all fields
	var fullItems []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fullItems); err != nil {
		return json.RawMessage("[]")
	}

	var filtered []json.RawMessage
	for i, item := range items {
		if item.Visibility != "admin" {
			filtered = append(filtered, fullItems[i])
		}
	}
	if len(filtered) == 0 {
		return json.RawMessage("[]")
	}
	result, err := json.Marshal(filtered)
	if err != nil {
		return json.RawMessage("[]")
	}
	return result
}

// safeRawJSONArray returns raw as json.RawMessage if it's valid JSON, otherwise "[]".
func safeRawJSONArray(raw string) json.RawMessage {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return json.RawMessage("[]")
	}
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	return json.RawMessage("[]")
}

// GetFrameSrcOrigins returns deduplicated http(s) origins from home_content URL,
// purchase_subscription_url, and all custom_menu_items URLs. Used by the router layer for CSP frame-src injection.
func (s *SettingService) GetFrameSrcOrigins(ctx context.Context) ([]string, error) {
	settings, err := s.GetPublicSettings(ctx)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var origins []string

	addOrigin := func(rawURL string) {
		if origin := extractOriginFromURL(rawURL); origin != "" {
			if _, ok := seen[origin]; !ok {
				seen[origin] = struct{}{}
				origins = append(origins, origin)
			}
		}
	}

	// home content URL (when home_content is set to a URL for iframe embedding)
	addOrigin(settings.HomeContent)

	// purchase subscription URL
	if settings.PurchaseSubscriptionEnabled {
		addOrigin(settings.PurchaseSubscriptionURL)
	}

	// all custom menu items (including admin-only, since CSP must allow all iframes)
	for _, item := range parseCustomMenuItemURLs(settings.CustomMenuItems) {
		addOrigin(item)
	}

	return origins, nil
}

// extractOriginFromURL returns the scheme+host origin from rawURL.
// Only http and https schemes are accepted.
func extractOriginFromURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// parseCustomMenuItemURLs extracts URLs from a raw JSON array of custom menu items.
func parseCustomMenuItemURLs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var items []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil
	}
	urls := make([]string, 0, len(items))
	for _, item := range items {
		if item.URL != "" {
			urls = append(urls, item.URL)
		}
	}
	return urls
}
