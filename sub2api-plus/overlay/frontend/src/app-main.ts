import type { App as VueApp } from 'vue'
import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
import i18n, { initI18n } from './i18n'
import { useAppStore } from '@/stores/app'
import { markFullAppActive } from '@/public/fullAppBridge'
import { updateFavicon } from '@/utils/branding'
import './style.css'
import './style-fork.css'

function initThemeClass() {
  const savedTheme = localStorage.getItem('theme')
  const shouldUseDark =
    savedTheme === 'dark' ||
    (!savedTheme && window.matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.classList.toggle('dark', shouldUseDark)
}

let mountedApp: VueApp<Element> | null = null

export async function mountFullApp(selector = '#app'): Promise<VueApp<Element>> {
  if (mountedApp) {
    return mountedApp
  }

  initThemeClass()
  markFullAppActive()

  const app = createApp(App)
  const pinia = createPinia()
  app.use(pinia)

  const appStore = useAppStore()
  appStore.initFromInjectedConfig()

  if (appStore.siteName) {
    document.title = `${appStore.siteName} - Secure Portal`
  }
  updateFavicon(appStore.siteLogo)

  await initI18n()

  app.use(router)
  app.use(i18n)

  await router.isReady()
  app.mount(selector)
  mountedApp = app
  return mountedApp
}
