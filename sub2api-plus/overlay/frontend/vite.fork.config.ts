import { defineConfig, loadEnv, mergeConfig } from 'vite'
import upstreamConfig from './vite.config'
import { createForkViteConfig } from './vite.fork'

export default defineConfig(async (configEnv) => {
  const resolvedUpstream = typeof upstreamConfig === 'function'
    ? await upstreamConfig(configEnv)
    : await upstreamConfig
  const env = loadEnv(configEnv.mode, process.cwd(), '')
  const backendUrl = env.VITE_DEV_PROXY_TARGET || 'http://localhost:8080'

  return mergeConfig(resolvedUpstream, createForkViteConfig(backendUrl))
})
