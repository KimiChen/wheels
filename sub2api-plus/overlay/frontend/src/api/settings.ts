import { apiClient } from './client'
import type { ClientEndpointSettings } from '@/types'

export async function getClientEndpoints(): Promise<ClientEndpointSettings> {
  const { data } = await apiClient.get<ClientEndpointSettings>('/settings/client-endpoints')
  return data
}

export const settingsAPI = {
  getClientEndpoints,
}

export default settingsAPI
