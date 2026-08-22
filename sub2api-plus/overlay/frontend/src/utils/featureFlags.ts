/**
 * Feature flag registry — single source of truth for public-settings-driven
 * feature switches used by the sidebar, routes, and views.
 *
 * ## Why this module exists
 *
 * `public settings` reach the frontend through two channels:
 *
 *   1. **SSR injection** — the backend embeds `window.__STATIC_APP__` into the
 *      HTML. `main.ts` calls `appStore.initFromInjectedConfig()` synchronously
 *      before Vue mounts, so `cachedPublicSettings` is populated on first
 *      render.
 *   2. **Async API** — `App.vue` awaits `appStore.fetchPublicSettings()` on
 *      mount as a fallback (used when injection is missing or stale).
 *
 * The HTML injection is intentionally sparse: disabled/default fields are not
 * emitted. Missing feature flags therefore resolve to `false`.
 *
 * ## Modes
 *
 *   - **`opt-out`** — legacy label for features that may be enabled in admin.
 *   - **`opt-in`**  — legacy label for features that are usually hidden.
 *
 * Both modes now fail closed in public HTML when the key is absent.
 *
 * ## Adding a new flag
 *
 *   1. Backend `service/domain_constants.go`  → `SettingKey<Name>Enabled`
 *   2. Backend `service/settings_view.go`      → `PublicSettings` + `SystemSettings`
 *   3. Backend `service/setting_service.go`    → `GetPublicSettings` / `UpdateSettings` /
 *                                                 `GetAllSettings` / `InitDefaultSettings`
 *   4. Backend `handler/dto/settings.go`       → `PublicSettings` + `SystemSettings`
 *   5. Backend `handler/setting_handler.go`    → handler response
 *   6. Backend `handler/admin/setting_handler.go` → update request + audit diff
 *   7. Frontend `types/index.ts`               → `PublicSettings` typings
 *   8. Frontend `api/admin/settings.ts`        → admin DTO typings
 *   9. **Frontend `utils/featureFlags.ts` (this file)** → register via `defineFlag`
 *  10. Frontend `views/admin/SettingsView.vue` → Toggle UI + form defaults + save payload
 *  11. Frontend `components/layout/AppSidebar.vue` → attach via `makeSidebarFlag`
 *
 * ## Usage
 *
 * ```ts
 * import { FeatureFlags, makeSidebarFlag } from '@/utils/featureFlags'
 *
 * const flagAvailableChannels = makeSidebarFlag(FeatureFlags.availableChannels)
 * // ...
 * { path: '/available-channels', label: ..., featureFlag: flagAvailableChannels }
 * ```
 *
 * `isFeatureFlagEnabled(flag)` returns the resolved boolean (`true` = show).
 * `makeSidebarFlag(flag)` returns a `() => boolean | undefined` compatible with
 * `AppSidebar.NavItem.featureFlag`, where `false` hides the menu entry.
 */
import { useAppStore } from '@/stores/app'
import type { PublicSettings } from '@/types'
import { DEFAULT_INTERVAL_SECONDS } from '@/constants/channelMonitor'

export type FeatureFlagMode = 'opt-in' | 'opt-out'

export interface FeatureFlagDefinition {
  /** Public-settings key used for lookup. */
  readonly key: keyof PublicSettings
  /** Resolution mode when the key is missing/undefined. */
  readonly mode: FeatureFlagMode
  /** Short human label for logs and debug tooling. */
  readonly label: string
}

function defineFlag<K extends keyof PublicSettings>(
  def: { key: K; mode: FeatureFlagMode; label: string },
): FeatureFlagDefinition {
  return def
}

/**
 * Registered feature flags. Add a new entry here when introducing a new
 * public-settings-driven switch; see the "Adding a new flag" checklist above.
 */
export const FeatureFlags = {
  channelMonitor: defineFlag({
    key: 'channel_monitor_enabled',
    mode: 'opt-out',
    label: 'Channel Monitor',
  }),
  availableChannels: defineFlag({
    key: 'available_channels_enabled',
    mode: 'opt-in',
    label: 'Available Channels',
  }),
  modelPlaza: defineFlag({
    key: 'model_plaza_enabled',
    mode: 'opt-in',
    label: 'Model Plaza',
  }),
  payment: defineFlag({
    key: 'payment_enabled',
    mode: 'opt-out',
    label: 'Payment',
  }),
  riskControl: defineFlag({
    key: 'risk_control_enabled',
    mode: 'opt-in',
    label: 'Risk Control',
  }),
  affiliate: defineFlag({
    key: 'affiliate_enabled',
    mode: 'opt-in',
    label: 'Affiliate',
  }),
} as const

export type RegisteredFeatureFlag = keyof typeof FeatureFlags

/**
 * Read the current value of a flag, honoring the mode's fallback.
 * `true`  → the feature is enabled (menu/route should render).
 * `false` → the feature is disabled (menu/route should hide).
 */
export function isFeatureFlagEnabled(flag: FeatureFlagDefinition): boolean {
  const appStore = useAppStore()
  const raw = appStore.cachedPublicSettings?.[flag.key] as
    | boolean
    | undefined
  if (typeof raw === 'boolean') return raw
  // Missing keys are treated as disabled because the SSR config is sparse.
  return false
}

/**
 * Sidebar NavItem.featureFlag accepts a getter that returns
 * `false` to hide. Keeping the same contract lets callers swap in
 * registry-backed flags without changing AppSidebar's filter logic.
 */
export function makeSidebarFlag(flag: FeatureFlagDefinition): () => boolean {
  return () => isFeatureFlagEnabled(flag)
}

/** True when channel monitor feature flag is enabled. */
export function isChannelMonitorRouteEnabled(): boolean {
  return isFeatureFlagEnabled(FeatureFlags.channelMonitor)
}

export type ChannelMonitorMode = 'v1' | 'v2'

/** Exclusive channel-monitor implementation. Invalid/missing → v1 (opt-in to v2). */
export function getChannelMonitorMode(): ChannelMonitorMode {
  const appStore = useAppStore()
  const mode = appStore.cachedPublicSettings?.channel_monitor_mode
  return mode === 'v2' ? 'v2' : 'v1'
}

export function isChannelMonitorV1Mode(): boolean {
  return isChannelMonitorRouteEnabled() && getChannelMonitorMode() === 'v1'
}

export function isChannelMonitorV2Mode(): boolean {
  return isChannelMonitorRouteEnabled() && getChannelMonitorMode() === 'v2'
}

export function getChannelMonitorRefreshIntervalSeconds(): number {
  const appStore = useAppStore()
  const configured = appStore.cachedPublicSettings?.channel_monitor_default_interval_seconds
  return configured && configured > 0 ? configured : DEFAULT_INTERVAL_SECONDS
}

/** Hide RPM/TPM on user-facing monitor (scale privacy). Admin always shows full metrics. */
export function isChannelMonitorThroughputHidden(): boolean {
  const appStore = useAppStore()
  return Boolean(appStore.cachedPublicSettings?.channel_monitor_hide_throughput)
}

/**
 * Show quota/balance snapshots on the user-facing monitor page
 * (channel_monitor_show_quota, default off). The backend strips
 * latest_quota server-side when the switch is off; this flag is
 * defense-in-depth only. Admin views always show quota.
 */
export function isChannelMonitorQuotaVisible(): boolean {
  const appStore = useAppStore()
  return appStore.cachedPublicSettings?.channel_monitor_show_quota === true
}
