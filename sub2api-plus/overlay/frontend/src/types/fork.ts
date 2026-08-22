/**
 * fork 自有类型（从 types/index.ts 抽离，避免与上游在同一位置追加类型时冲突）。
 *
 * 约定：这里只存放 fork 新增的独立类型；
 * 对上游接口（UsageLog / DashboardStats / TrendDataPoint 等）的字段追加
 * 仍保留在 index.ts 接口体内——追加式小改动合并风险低，
 * 且字段与接口同处更易读。
 */
import type { CustomEndpoint, PublicSettings } from './index'

export type PublicSettingsConfig = Partial<PublicSettings>

export interface ClientEndpointSettings {
  site_name: string
  api_base_url: string
  custom_endpoints: CustomEndpoint[]
  hide_ccs_import_button: boolean
}
