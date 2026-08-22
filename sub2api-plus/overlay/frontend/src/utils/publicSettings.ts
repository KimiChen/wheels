import type { PublicSettings, PublicSettingsConfig } from '@/types'

export const DEFAULT_PUBLIC_SITE_NAME = '企业数据中台'
export const DEFAULT_PUBLIC_SITE_SUBTITLE = '统一数据目录、治理与服务编排入口'
const LEGACY_DEFAULT_SITE_NAME_CODES = [83, 117, 98, 50, 65, 80, 73] as const
const LEGACY_DEFAULT_SITE_SUBTITLE_CODES = [
  83, 117, 98, 115, 99, 114, 105, 112, 116, 105, 111, 110, 32, 116, 111, 32, 65,
  80, 73, 32, 67, 111, 110, 118, 101, 114, 115, 105, 111, 110, 32, 80, 108, 97,
  116, 102, 111, 114, 109,
] as const

const fromCharCodes = (codes: readonly number[]): string => String.fromCharCode(...codes)

export const isLegacyDefaultSiteName = (value: string): boolean =>
  value === fromCharCodes(LEGACY_DEFAULT_SITE_NAME_CODES)

const isLegacyDefaultSiteSubtitle = (value: string): boolean =>
  value === fromCharCodes(LEGACY_DEFAULT_SITE_SUBTITLE_CODES)

export const createDefaultPublicSettings = (): PublicSettings => ({
  registration_enabled: false,
  email_verify_enabled: false,
  force_email_on_third_party_signup: false,
  registration_email_suffix_whitelist: [],
  registration_email_domain_quota_enabled: false,
  promo_code_enabled: false,
  password_reset_enabled: false,
  invitation_code_enabled: false,
  login_agreement_enabled: false,
  login_agreement_mode: 'modal',
  login_agreement_updated_at: '',
  login_agreement_revision: '',
  login_agreement_documents: [],
  turnstile_enabled: false,
  tencent_captcha_enabled: false,
  tencent_captcha_app_id: '',
  tencent_captcha_region: 'cn',
  passkey_enabled: false,
  turnstile_site_key: '',
  aliyun_captcha_enabled: false,
  aliyun_captcha_scene_id: '',
  aliyun_captcha_prefix: '',
  aliyun_captcha_region: 'cn',
  site_name: DEFAULT_PUBLIC_SITE_NAME,
  site_logo: '',
  site_subtitle: DEFAULT_PUBLIC_SITE_SUBTITLE,
  api_base_url: '',
  contact_info: '',
  doc_url: '',
  home_content: '',
  compact_home_enabled: false,
  hide_ccs_import_button: false,
  payment_enabled: false,
  risk_control_enabled: false,
  table_default_page_size: 20,
  table_page_size_options: [10, 20, 50, 100],
  custom_menu_items: [],
  custom_endpoints: [],
  linuxdo_oauth_enabled: false,
  dingtalk_oauth_enabled: false,
  wechat_oauth_enabled: false,
  wechat_oauth_open_enabled: false,
  wechat_oauth_mp_enabled: false,
  wechat_oauth_mobile_enabled: false,
  oidc_oauth_enabled: false,
  oidc_oauth_provider_name: 'OIDC',
  github_oauth_enabled: false,
  google_oauth_enabled: false,
  backend_mode_enabled: false,
  version: '',
  balance_low_notify_enabled: false,
  account_quota_notify_enabled: false,
  balance_low_notify_threshold: 0,
  channel_monitor_enabled: false,
  channel_monitor_mode: 'v1',
  channel_monitor_default_interval_seconds: 60,
  channel_monitor_hide_throughput: true,
  available_channels_enabled: false,
  model_plaza_enabled: false,
  model_plaza_require_auth: false,
  service_quota_enabled: false,
  affiliate_enabled: false,
  allow_user_view_error_requests: false,
})

export const normalizePublicSettings = (
  config: PublicSettingsConfig | null | undefined,
): PublicSettings => ({
  ...createDefaultPublicSettings(),
  ...(config ?? {}),
  registration_email_suffix_whitelist: Array.isArray(config?.registration_email_suffix_whitelist)
    ? config.registration_email_suffix_whitelist
    : [],
  login_agreement_documents: Array.isArray(config?.login_agreement_documents)
    ? config.login_agreement_documents
    : [],
  table_page_size_options: Array.isArray(config?.table_page_size_options)
    ? config.table_page_size_options
    : [10, 20, 50, 100],
  custom_menu_items: Array.isArray(config?.custom_menu_items)
    ? config.custom_menu_items
    : [],
  custom_endpoints: Array.isArray(config?.custom_endpoints)
    ? config.custom_endpoints
    : [],
})

const addTrueSetting = (
  out: PublicSettingsConfig,
  key: keyof PublicSettings,
  value: boolean | undefined,
): void => {
  if (value === true) {
    out[key] = true as never
  }
}

const addStringSetting = (
  out: PublicSettingsConfig,
  key: keyof PublicSettings,
  value: string | undefined,
  shouldOmit?: (value: string) => boolean,
): void => {
  const trimmed = value?.trim()
  if (!trimmed || shouldOmit?.(trimmed)) return
  out[key] = trimmed as never
}

export const compactPublicSettingsConfig = (
  config: PublicSettingsConfig | null | undefined,
): PublicSettingsConfig => {
  const normalized = normalizePublicSettings(config)
  const out: PublicSettingsConfig = {}

  addStringSetting(out, 'site_name', normalized.site_name, isLegacyDefaultSiteName)
  addStringSetting(out, 'site_logo', normalized.site_logo)
  addStringSetting(out, 'site_subtitle', normalized.site_subtitle, isLegacyDefaultSiteSubtitle)
  addStringSetting(out, 'contact_info', normalized.contact_info)
  addStringSetting(out, 'doc_url', normalized.doc_url)
  addStringSetting(out, 'home_content', normalized.home_content)

  addTrueSetting(out, 'registration_enabled', normalized.registration_enabled)
  if (normalized.registration_enabled) {
    addTrueSetting(out, 'email_verify_enabled', normalized.email_verify_enabled)
    if (normalized.registration_email_suffix_whitelist.length > 0) {
      out.registration_email_suffix_whitelist = normalized.registration_email_suffix_whitelist
    }
    addTrueSetting(
      out,
      'registration_email_domain_quota_enabled',
      normalized.registration_email_domain_quota_enabled,
    )
    addTrueSetting(out, 'promo_code_enabled', normalized.promo_code_enabled)
    addTrueSetting(out, 'invitation_code_enabled', normalized.invitation_code_enabled)
  }
  addTrueSetting(out, 'password_reset_enabled', normalized.password_reset_enabled)

  if (
    normalized.login_agreement_enabled &&
    (normalized.login_agreement_documents?.length ?? 0) > 0
  ) {
    out.login_agreement_enabled = true
    out.login_agreement_mode = normalized.login_agreement_mode
    out.login_agreement_updated_at = normalized.login_agreement_updated_at
    out.login_agreement_revision = normalized.login_agreement_revision
    out.login_agreement_documents = normalized.login_agreement_documents
  }

  if (normalized.turnstile_enabled) {
    out.turnstile_enabled = true
    addStringSetting(out, 'turnstile_site_key', normalized.turnstile_site_key)
  }
  if (normalized.tencent_captcha_enabled) {
    out.tencent_captcha_enabled = true
    addStringSetting(out, 'tencent_captcha_app_id', normalized.tencent_captcha_app_id)
    addStringSetting(out, 'tencent_captcha_region', normalized.tencent_captcha_region)
  }
  if (normalized.aliyun_captcha_enabled) {
    out.aliyun_captcha_enabled = true
    addStringSetting(out, 'aliyun_captcha_scene_id', normalized.aliyun_captcha_scene_id)
    addStringSetting(out, 'aliyun_captcha_prefix', normalized.aliyun_captcha_prefix)
    addStringSetting(out, 'aliyun_captcha_region', normalized.aliyun_captcha_region)
  }
  addTrueSetting(out, 'passkey_enabled', normalized.passkey_enabled)

  addTrueSetting(out, 'linuxdo_oauth_enabled', normalized.linuxdo_oauth_enabled)
  addTrueSetting(out, 'dingtalk_oauth_enabled', normalized.dingtalk_oauth_enabled)
  addTrueSetting(out, 'wechat_oauth_enabled', normalized.wechat_oauth_enabled)
  addTrueSetting(out, 'wechat_oauth_open_enabled', normalized.wechat_oauth_open_enabled)
  addTrueSetting(out, 'wechat_oauth_mp_enabled', normalized.wechat_oauth_mp_enabled)
  addTrueSetting(out, 'wechat_oauth_mobile_enabled', normalized.wechat_oauth_mobile_enabled)
  if (normalized.oidc_oauth_enabled) {
    out.oidc_oauth_enabled = true
    addStringSetting(out, 'oidc_oauth_provider_name', normalized.oidc_oauth_provider_name)
  }
  addTrueSetting(out, 'github_oauth_enabled', normalized.github_oauth_enabled)
  addTrueSetting(out, 'google_oauth_enabled', normalized.google_oauth_enabled)
  addTrueSetting(out, 'backend_mode_enabled', normalized.backend_mode_enabled)
  addTrueSetting(out, 'payment_enabled', normalized.payment_enabled)
  addTrueSetting(out, 'compact_home_enabled', normalized.compact_home_enabled)
  if (normalized.channel_monitor_enabled) {
    out.channel_monitor_enabled = true
    if (normalized.channel_monitor_mode === 'v2') {
      out.channel_monitor_mode = 'v2'
    }
    if (normalized.channel_monitor_default_interval_seconds !== 60) {
      out.channel_monitor_default_interval_seconds = normalized.channel_monitor_default_interval_seconds
    }
    addTrueSetting(
      out,
      'channel_monitor_hide_throughput',
      normalized.channel_monitor_hide_throughput,
    )
  }
  addTrueSetting(out, 'available_channels_enabled', normalized.available_channels_enabled)
  if (normalized.model_plaza_enabled) {
    out.model_plaza_enabled = true
    addTrueSetting(out, 'model_plaza_require_auth', normalized.model_plaza_require_auth)
  }
  addTrueSetting(out, 'affiliate_enabled', normalized.affiliate_enabled)
  addTrueSetting(out, 'risk_control_enabled', normalized.risk_control_enabled)

  return out
}

export const getInjectedPublicSettings = (): PublicSettingsConfig | null => {
  if (typeof window === 'undefined') return null
  return window.__STATIC_APP__ ?? window.__APP_CONFIG__ ?? null
}

export const writeInjectedPublicSettings = (
  config: PublicSettingsConfig | null | undefined,
): void => {
  if (typeof window === 'undefined') return
  window.__STATIC_APP__ = compactPublicSettingsConfig(config)
  delete window.__APP_CONFIG__
}
