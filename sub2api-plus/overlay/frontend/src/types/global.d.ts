import type { PublicSettingsConfig } from '@/types'

declare global {
  interface Window {
    __STATIC_APP__?: PublicSettingsConfig
    __APP_CONFIG__?: PublicSettingsConfig
  }
}

export {}
