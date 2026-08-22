import { describe, expect, it } from 'vitest'
import {
  createForkViteConfig,
  forkPublicSettingsPlugin,
  manualChunks
} from '../../vite.fork'

describe('fork Vite configuration', () => {
  it('keeps fork-only build and proxy settings outside the upstream config', () => {
    const config = createForkViteConfig('http://backend.test') as any

    expect(config.base).toBe('/static/app/')
    expect(config.server.proxy['/user']).toEqual({
      target: 'http://backend.test',
      changeOrigin: true
    })
    expect(config.build.rollupOptions.output).toMatchObject({
      entryFileNames: 'res/[hash].js',
      chunkFileNames: 'res/[hash].js',
      assetFileNames: 'res/[hash][extname]',
      manualChunks
    })
  })

  it('adapts upstream development-time public settings to fork globals and branding', () => {
    const plugin = forkPublicSettingsPlugin()
    const hook = plugin.transformIndexHtml as any
    const html = '<title>Example - AI API Gateway</title><script>window.__APP_CONFIG__={};</script>'

    expect(hook.handler(html)).toBe(
      '<title>Example - Secure Portal</title><script>window.__STATIC_APP__={};</script>'
    )
  })

  it('keeps Pinia separate while preserving the fork chunk strategy', () => {
    expect(manualChunks('/repo/node_modules/pinia/index.js')).toBe('vendor-store')
    expect(manualChunks('/repo/node_modules/vue/dist/vue.js')).toBe('vendor-vue')
    expect(manualChunks('/repo/node_modules/tiny-package/index.js')).toBeUndefined()
  })
})
