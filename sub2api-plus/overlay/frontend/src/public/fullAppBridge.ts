import type { App as VueApp } from 'vue'
import type { Router } from 'vue-router'

let publicApp: VueApp<Element> | null = null
let fullAppActive = false
let pendingMount: Promise<unknown> | null = null

function currentPath(): string {
  return `${window.location.pathname}${window.location.search}${window.location.hash}`
}

export function setPublicApp(app: VueApp<Element>): void {
  publicApp = app
}

export function markFullAppActive(): void {
  fullAppActive = true
}

export function isFullAppActive(): boolean {
  return fullAppActive
}

export async function enterFullApp(targetPath = currentPath()): Promise<void> {
  if (targetPath && targetPath !== currentPath()) {
    window.history.replaceState(null, '', targetPath)
  }

  if (fullAppActive) {
    return
  }

  if (!pendingMount) {
    pendingMount = (async () => {
      if (publicApp) {
        publicApp.unmount()
        publicApp = null
      }

      const container = document.querySelector('#app')
      if (container) {
        container.innerHTML = ''
      }

      const { mountFullApp } = await import('@/app-main')
      await mountFullApp('#app')
    })()
  }

  await pendingMount
}

export async function navigateToAuthenticatedApp(
  router: Router,
  targetPath: string,
): Promise<void> {
  if (isFullAppActive()) {
    await router.push(targetPath)
    return
  }

  await enterFullApp(targetPath)
}
