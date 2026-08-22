/**
 * Application State Store
 * Manages global UI state including sidebar, loading indicators, and toast notifications
 */

import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import type { Toast, ToastType, PublicSettings, PublicSettingsConfig } from '@/types'
import { i18n } from '@/i18n'
import type { VersionInfo, ReleaseInfo } from '@/api/admin/system'
import { getPublicSettings as fetchPublicSettingsAPI } from '@/api/publicAuth'
import {
  DEFAULT_PUBLIC_SITE_NAME,
  createDefaultPublicSettings,
  getInjectedPublicSettings,
  normalizePublicSettings,
  writeInjectedPublicSettings,
} from '@/utils/publicSettings'

export const useAppStore = defineStore('app', () => {
  // ==================== State ====================

  const sidebarCollapsed = ref<boolean>(false)
  const mobileOpen = ref<boolean>(false)
  const sidebarScrollTop = ref<number>(0)
  const loading = ref<boolean>(false)
  const toasts = ref<Toast[]>([])

  // Public settings cache state
  const publicSettingsLoaded = ref<boolean>(false)
  const publicSettingsLoading = ref<boolean>(false)
  const siteName = ref<string>(DEFAULT_PUBLIC_SITE_NAME)
  const siteLogo = ref<string>('')
  const siteVersion = ref<string>('')
  const contactInfo = ref<string>('')
  const apiBaseUrl = ref<string>('')
  const docUrl = ref<string>('')
  const cachedPublicSettings = ref<PublicSettings | null>(null)
  let publicSettingsRequest: Promise<PublicSettings | null> | null = null

  // Version cache state
  const versionLoaded = ref<boolean>(false)
  const versionLoading = ref<boolean>(false)
  const currentVersion = ref<string>('')
  const upstreamCurrentVersion = ref<string>('')
  const latestVersion = ref<string>('')
  const hasUpdate = ref<boolean>(false)
  const buildType = ref<string>('source')
  const forkBuild = ref<boolean>(false)
  const releaseInfo = ref<ReleaseInfo | null>(null)

  // Auto-incrementing ID for toasts
  let toastIdCounter = 0

  // ==================== Computed ====================

  const hasActiveToasts = computed(() => toasts.value.length > 0)
  const backendModeEnabled = computed(() => cachedPublicSettings.value?.backend_mode_enabled ?? false)

  const loadingCount = ref<number>(0)

  // ==================== Actions ====================

  /**
   * Toggle sidebar collapsed state
   */
  function toggleSidebar(): void {
    sidebarCollapsed.value = !sidebarCollapsed.value
  }

  /**
   * Set sidebar collapsed state explicitly
   * @param collapsed - Whether sidebar should be collapsed
   */
  function setSidebarCollapsed(collapsed: boolean): void {
    sidebarCollapsed.value = collapsed
  }

  /**
   * Toggle mobile sidebar open state
   */
  function toggleMobileSidebar(): void {
    mobileOpen.value = !mobileOpen.value
  }

  /**
   * Set mobile sidebar open state explicitly
   * @param open - Whether mobile sidebar should be open
   */
  function setMobileOpen(open: boolean): void {
    mobileOpen.value = open
  }

  /**
   * Set global loading state
   * @param isLoading - Whether app is in loading state
   */
  function setLoading(isLoading: boolean): void {
    if (isLoading) {
      loadingCount.value++
    } else {
      loadingCount.value = Math.max(0, loadingCount.value - 1)
    }
    loading.value = loadingCount.value > 0
  }

  /**
   * Show a toast notification
   * @param type - Type of toast (success, error, info, warning)
   * @param message - Toast message content
   * @param duration - Auto-dismiss duration in ms (undefined = no auto-dismiss)
   * @returns Toast ID for manual dismissal
   */
  function showToast(type: ToastType, message: string, duration?: number): string {
    const id = `toast-${++toastIdCounter}`
    const toast: Toast = {
      id,
      type,
      message,
      duration,
      startTime: duration !== undefined ? Date.now() : undefined
    }

    toasts.value.push(toast)

    // Auto-dismiss if duration is specified
    if (duration !== undefined) {
      setTimeout(() => {
        hideToast(id)
      }, duration)
    }

    return id
  }

  /**
   * Show a success toast
   * @param message - Success message
   * @param duration - Auto-dismiss duration in ms (default: 3000)
   */
  function showSuccess(message: string, duration: number = 3000): string {
    return showToast('success', message, duration)
  }

  /**
   * Show an error toast
   * @param message - Error message
   * @param duration - Auto-dismiss duration in ms (default: 5000)
   */
  function showError(message: string, duration: number = 5000): string {
    return showToast('error', message, duration)
  }

  /**
   * Show an info toast
   * @param message - Info message
   * @param duration - Auto-dismiss duration in ms (default: 3000)
   */
  function showInfo(message: string, duration: number = 3000): string {
    return showToast('info', message, duration)
  }

  /**
   * Show a warning toast
   * @param message - Warning message
   * @param duration - Auto-dismiss duration in ms (default: 4000)
   */
  function showWarning(message: string, duration: number = 4000): string {
    return showToast('warning', message, duration)
  }

  /**
   * Hide a specific toast by ID
   * @param id - Toast ID to hide
   */
  function hideToast(id: string): void {
    const index = toasts.value.findIndex((t) => t.id === id)
    if (index !== -1) {
      toasts.value.splice(index, 1)
    }
  }

  /**
   * Clear all toasts
   */
  function clearAllToasts(): void {
    toasts.value = []
  }

  /**
   * Execute an async operation with loading state
   * Automatically manages loading indicator
   * @param operation - Async operation to execute
   * @returns Promise resolving to operation result
   */
  async function withLoading<T>(operation: () => Promise<T>): Promise<T> {
    setLoading(true)
    try {
      return await operation()
    } finally {
      setLoading(false)
    }
  }

  /**
   * Execute an async operation with loading and error handling
   * Shows error toast on failure
   * @param operation - Async operation to execute
   * @param errorMessage - Custom error message (optional)
   * @returns Promise resolving to operation result or null on error
   */
  async function withLoadingAndError<T>(
    operation: () => Promise<T>,
    errorMessage?: string
  ): Promise<T | null> {
    setLoading(true)
    try {
      return await operation()
    } catch (error) {
      const message =
        errorMessage ||
        (error as { message?: string }).message ||
        i18n.global.t('common.unknownError')
      showError(message)
      return null
    } finally {
      setLoading(false)
    }
  }

  /**
   * Reset app state to defaults
   * Useful for cleanup or testing
   */
  function reset(): void {
    sidebarCollapsed.value = false
    loading.value = false
    loadingCount.value = 0
    toasts.value = []
  }

  // ==================== Version Management ====================

  /**
   * Fetch version info (uses cache unless force=true)
   * @param force - Force refresh from API
   */
  async function fetchVersion(force = false): Promise<VersionInfo | null> {
    // Return cached data if available and not forcing refresh
    if (versionLoaded.value && !force) {
      return {
        current_version: currentVersion.value,
        upstream_current_version: upstreamCurrentVersion.value,
        latest_version: latestVersion.value,
        has_update: hasUpdate.value,
        build_type: buildType.value,
        fork_build: forkBuild.value,
        release_info: releaseInfo.value || undefined,
        cached: true
      }
    }

    // Prevent duplicate requests
    if (versionLoading.value) {
      return null
    }

    versionLoading.value = true
    try {
      const { checkUpdates: checkUpdatesAPI } = await import('@/api/admin/system')
      const data = await checkUpdatesAPI(force)
      currentVersion.value = data.current_version
      upstreamCurrentVersion.value = data.upstream_current_version || data.current_version
      latestVersion.value = data.latest_version
      hasUpdate.value = data.has_update
      buildType.value = data.build_type || 'source'
      forkBuild.value = data.fork_build || false
      releaseInfo.value = data.release_info || null
      versionLoaded.value = true
      return data
    } catch (error) {
      console.error('Failed to fetch version:', error)
      return null
    } finally {
      versionLoading.value = false
    }
  }

  /**
   * Clear version cache (e.g., after update)
   */
  function clearVersionCache(): void {
    versionLoaded.value = false
    hasUpdate.value = false
  }

  // ==================== Public Settings Management ====================

  /**
   * Apply settings to store state (internal helper to avoid code duplication)
   */
  function applySettings(config: PublicSettingsConfig): PublicSettings {
    const normalized = normalizePublicSettings(config)
    writeInjectedPublicSettings(config)
    cachedPublicSettings.value = normalized
    siteName.value = normalized.site_name || DEFAULT_PUBLIC_SITE_NAME
    siteLogo.value = normalized.site_logo || ''
    siteVersion.value = normalized.version || ''
    contactInfo.value = normalized.contact_info || ''
    apiBaseUrl.value = normalized.api_base_url || ''
    docUrl.value = normalized.doc_url || ''
    publicSettingsLoaded.value = true
    return normalized
  }

  /**
   * Fetch public settings (uses cache unless force=true)
   * @param force - Force refresh from API
   */
  function fetchPublicSettings(force = false): Promise<PublicSettings | null> {
    // An active request always wins over cache/force semantics so every caller observes
    // the same refresh result and no older request can overwrite a newer one.
    if (publicSettingsRequest) {
      return publicSettingsRequest
    }

    const injectedConfig = getInjectedPublicSettings()
    if (!publicSettingsLoaded.value && !force && injectedConfig) {
      return Promise.resolve(applySettings(injectedConfig))
    }

    // Return cached data if available and not forcing refresh
    if (publicSettingsLoaded.value && !force) {
      if (cachedPublicSettings.value) {
        return Promise.resolve({ ...cachedPublicSettings.value })
      }
      return Promise.resolve({
        ...createDefaultPublicSettings(),
        site_name: siteName.value,
        site_logo: siteLogo.value,
        api_base_url: apiBaseUrl.value,
        contact_info: contactInfo.value,
        doc_url: docUrl.value,
        version: siteVersion.value,
      })
    }

    publicSettingsLoading.value = true
    let apiRequest: Promise<PublicSettings>
    try {
      apiRequest = fetchPublicSettingsAPI()
    } catch (error) {
      console.error('Failed to fetch public settings:', error)
      publicSettingsLoading.value = false
      return Promise.resolve(null)
    }

    const request = apiRequest
      .then((data) => {
        return applySettings(data)
      })
      .catch((error) => {
        console.error('Failed to fetch public settings:', error)
        return null
      })
      .finally(() => {
        if (publicSettingsRequest === request) {
          publicSettingsRequest = null
          publicSettingsLoading.value = false
        }
      })

    publicSettingsRequest = request
    return request
  }

  /**
   * Clear public settings cache
   */
  function clearPublicSettingsCache(): void {
    publicSettingsLoaded.value = false
    cachedPublicSettings.value = null
  }

  /**
   * Initialize settings from injected config (window.__STATIC_APP__)
   * This is called synchronously before Vue app mounts to prevent flash
   * @returns true if config was found and applied, false otherwise
   */
  function initFromInjectedConfig(): boolean {
    const injectedConfig = getInjectedPublicSettings()
    if (injectedConfig) {
      applySettings(injectedConfig)
      return true
    }
    return false
  }

  // ==================== Return Store API ====================

  return {
    // State
    sidebarCollapsed,
    mobileOpen,
    sidebarScrollTop,
    loading,
    toasts,

    // Public settings state
    publicSettingsLoaded,
    siteName,
    siteLogo,
    siteVersion,
    contactInfo,
    apiBaseUrl,
    docUrl,
    cachedPublicSettings,

    // Version state
    versionLoaded,
    versionLoading,
    currentVersion,
    upstreamCurrentVersion,
    latestVersion,
    hasUpdate,
    buildType,
    forkBuild,
    releaseInfo,

    // Computed
    hasActiveToasts,
    backendModeEnabled,

    // Actions
    toggleSidebar,
    setSidebarCollapsed,
    toggleMobileSidebar,
    setMobileOpen,
    setLoading,
    showToast,
    showSuccess,
    showError,
    showInfo,
    showWarning,
    hideToast,
    clearAllToasts,
    withLoading,
    withLoadingAndError,
    reset,

    // Version actions
    fetchVersion,
    clearVersionCache,

    // Public settings actions
    fetchPublicSettings,
    clearPublicSettingsCache,
    initFromInjectedConfig
  }
})
