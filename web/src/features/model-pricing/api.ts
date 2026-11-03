/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import {
  useMutation,
  useQuery,
  useQueryClient,
  type QueryClient,
} from '@tanstack/react-query'
import { t } from 'i18next'

import { pluginExpressionsEqual } from '@/features/pricing/lib/plugin-pricing'
import type {
  BillingUsageSchema,
  BillingUsageExample,
} from '@/features/pricing/types'
import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { createServerError } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import {
  EXACT_PRICING_KEYS,
  PRICING_KEYS,
  pricingValuesByModel,
  SHARED_PRICING_KEYS,
  type PricingKey,
  type PricingOptions,
  type PricingValues,
  type CacheWriteMode,
  type LegacyBillingDetails,
  type SharedPricingKey,
} from './pricing'

export type ModelPricingDescription = {
  billing_details?: LegacyBillingDetails
  effective: PricingValues
  cache_write_mode?: CacheWriteMode
}

export type ModelPricingPluginVariant = {
  plugin_key: string
  plugin_name: string
  icon?: string
  usage_schema: BillingUsageSchema
  usage_examples?: BillingUsageExample[]
  configured: string
  effective: string
  compatible: boolean
  stale?: boolean
}

export type ModelPricingEntry = ModelPricingDescription & {
  plugin_variants?: ModelPricingPluginVariant[]
  model_name: string
  numeric_model_name?: string
  version: string
  configured: PricingValues
  usage_schema?: BillingUsageSchema
}

export type ModelPricingConfig = {
  entries: ModelPricingEntry[]
  options: PricingOptions
  empty_version: string
}
export type ModelPricingChange = {
  model_name: string
  expected_version: string
  pricing: PricingValues
  reset?: boolean
}

export type ModelPricingConversion = Partial<ModelPricingDescription> & {
  expression?: string
  unsupported_reason?: string
}

export async function previewModelPricingConversion(request: {
  model_name: string
  pricing: PricingValues
}): Promise<ModelPricingConversion> {
  const response = await api.post('/api/option/model_pricing/convert', request)
  if (!response.data.success) {
    throw createServerError(
      response.data,
      t('Failed to prepare pricing conversion')
    )
  }
  return response.data.data
}

export async function previewModelPricing(request: {
  model_name: string
  pricing: PricingValues
}): Promise<{
  effective: PricingValues
  cacheWriteMode?: CacheWriteMode
  billingDetails?: LegacyBillingDetails
}> {
  const response = await api.post('/api/option/model_pricing/preview', request)
  if (!response.data.success) {
    throw createServerError(response.data, t('Failed to load model pricing'))
  }
  return {
    effective: response.data.data.effective,
    cacheWriteMode: response.data.data.cache_write_mode,
    billingDetails: response.data.data.billing_details,
  }
}

export function useCanEditModelPricing() {
  return useAuthStore((state) => state.auth.user?.role === ROLE.SUPER_ADMIN)
}

export async function getModelPricing(
  names: string[] = []
): Promise<ModelPricingConfig> {
  const params = new URLSearchParams()
  for (const name of names) params.append('model', name)
  const res = await api.get('/api/option/model_pricing', { params })
  if (!res.data.success) {
    throw createServerError(res.data, t('Failed to load model pricing'))
  }
  return res.data.data
}

export function useModelPricing(names: string[] = [], enabled = true) {
  const canEdit = useCanEditModelPricing()
  return useQuery({
    queryKey: ['model-pricing-config', ...names],
    queryFn: () => getModelPricing(names),
    enabled: enabled && canEdit,
    refetchOnWindowFocus: false,
  })
}

export async function invalidateModelPricing(client: QueryClient) {
  await Promise.all([
    client.invalidateQueries({ queryKey: ['model-pricing-config'] }),
    client.invalidateQueries({ queryKey: ['model-pricing-preview'] }),
    client.invalidateQueries({ queryKey: ['system-options'] }),
    client.invalidateQueries({ queryKey: ['pricing'] }),
    client.invalidateQueries({ queryKey: ['models'] }),
  ])
}

export async function saveModelPricing(changes: ModelPricingChange[]) {
  if (!changes.length) return
  const res = await api.patch('/api/option/model_pricing', { changes })
  if (!res.data.success) {
    throw createServerError(res.data, t('Failed to save model pricing'))
  }
}

export function useSaveModelPricing() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: saveModelPricing,
    onSuccess: () => invalidateModelPricing(client),
  })
}

// Only dirty model fields are applied to stored configuration. Display-only
// built-in expressions for other models never become administrator overrides.
export function buildPricingChanges(
  snapshot: ModelPricingConfig,
  before: PricingOptions,
  after: PricingOptions,
  selectedModelNames?: Iterable<string>
): ModelPricingChange[] {
  const previous = pricingValuesByModel(before)
  const next = pricingValuesByModel(after)
  const entries = new Map(
    snapshot.entries.map((entry) => [entry.model_name, entry])
  )
  const allNames = new Set([...previous.keys(), ...next.keys()])

  if (selectedModelNames) {
    const selectedSet = new Set(selectedModelNames)
    const groupedSelected = new Map<string, PricingValues[]>()

    for (const name of selectedSet) {
      const values = next.get(name) ?? {}
      if (values['billing_setting.billing_mode'] === 'tiered_expr') {
        continue
      }
      const entry = entries.get(name)
      const aliasKey = entry?.numeric_model_name?.trim() || name
      let list = groupedSelected.get(aliasKey)
      if (!list) {
        list = []
        groupedSelected.set(aliasKey, list)
      }
      list.push(values)
    }

    for (const members of groupedSelected.values()) {
      if (members.length <= 1) continue
      const first = members[0]
      for (let i = 1; i < members.length; i++) {
        const other = members[i]
        for (const sharedKey of SHARED_PRICING_KEYS) {
          if (first[sharedKey] !== other[sharedKey]) {
            throw new Error(t('Reload pricing'))
          }
        }
      }
    }
  }

  type DirtyItem = {
    name: string
    entry?: ModelPricingEntry
    dirty: PricingKey[]
    newValues: PricingValues
  }

  const dirtyItems: DirtyItem[] = []
  for (const name of allNames) {
    const oldValues = previous.get(name) ?? {}
    const newValues = next.get(name) ?? {}
    const dirty = PRICING_KEYS.filter((key) =>
      key === 'billing_setting.plugin_billing_expr'
        ? !pluginExpressionsEqual(oldValues[key], newValues[key])
        : oldValues[key] !== newValues[key]
    )
    if (!dirty.length) continue
    dirtyItems.push({
      name,
      entry: entries.get(name),
      dirty,
      newValues,
    })
  }

  if (!dirtyItems.length) return []

  const sharedPatches = new Map<
    string,
    Map<SharedPricingKey, number | undefined>
  >()

  for (const item of dirtyItems) {
    const aliasKey = item.entry?.numeric_model_name ?? item.name
    for (const key of item.dirty) {
      if (!SHARED_PRICING_KEYS.includes(key as SharedPricingKey)) continue
      const sharedKey = key as SharedPricingKey
      const proposedValue = item.newValues[sharedKey] as number | undefined
      let aliasMap = sharedPatches.get(aliasKey)
      if (!aliasMap) {
        aliasMap = new Map()
        sharedPatches.set(aliasKey, aliasMap)
      }
      if (aliasMap.has(sharedKey)) {
        if (aliasMap.get(sharedKey) !== proposedValue) {
          throw new Error(t('Reload pricing'))
        }
      } else {
        aliasMap.set(sharedKey, proposedValue)
      }
    }
  }

  const changes: ModelPricingChange[] = []
  for (const item of dirtyItems) {
    const entry = item.entry
    const pricing: PricingValues = { ...entry?.configured }
    const aliasKey = entry?.numeric_model_name ?? item.name
    const aliasPatch = sharedPatches.get(aliasKey)

    for (const key of item.dirty) {
      if (SHARED_PRICING_KEYS.includes(key as SharedPricingKey)) continue
      delete pricing[key]
      if (key === 'billing_setting.plugin_billing_expr') {
        const val = item.newValues[key]
        if (val !== undefined) {
          pricing[key] = val
        }
      } else {
        const val = item.newValues[key]
        if (val !== undefined) {
          pricing[key] = val
        }
      }
    }

    if (aliasPatch) {
      for (const [key, value] of aliasPatch.entries()) {
        delete pricing[key]
        if (value !== undefined) {
          pricing[key] = value
        }
      }
    }

    if (item.newValues['billing_setting.billing_mode'] === 'tiered_expr') {
      pricing['billing_setting.billing_mode'] = 'tiered_expr'
      pricing['billing_setting.billing_expr'] =
        item.newValues['billing_setting.billing_expr']
    }

    changes.push({
      model_name: item.name,
      expected_version: entry?.version ?? snapshot.empty_version,
      pricing,
    })
  }

  return changes
}

export const PRICING_URL_BYTE_BUDGET = 4096
const PRICING_PATH_OVERHEAD = '/api/option/model_pricing?'.length
const MAX_QUERY_BYTES = PRICING_URL_BYTE_BUDGET - PRICING_PATH_OVERHEAD

function batchMissingModelNames(names: string[]): string[][] {
  const batches: string[][] = []
  let currentBatch: string[] = []
  let currentBytes = 0

  for (const name of names) {
    const encoded = new URLSearchParams([['model', name]]).toString()
    const encodedBytes = encoded.length
    if (PRICING_PATH_OVERHEAD + encodedBytes > PRICING_URL_BYTE_BUDGET) {
      throw new Error(t('Failed to load model pricing'))
    }

    const additionalBytes =
      currentBatch.length === 0 ? encodedBytes : 1 + encodedBytes
    if (
      currentBatch.length > 0 &&
      currentBytes + additionalBytes > MAX_QUERY_BYTES
    ) {
      batches.push(currentBatch)
      currentBatch = [name]
      currentBytes = encodedBytes
    } else {
      currentBatch.push(name)
      currentBytes += additionalBytes
    }
  }

  if (currentBatch.length > 0) {
    batches.push(currentBatch)
  }

  return batches
}

export async function resolvePricingSnapshot(
  snapshot: ModelPricingConfig,
  names: string[] = []
): Promise<ModelPricingConfig> {
  const knownEntries = new Map(
    snapshot.entries.map((entry) => [entry.model_name, entry])
  )
  const missingNames = [...new Set(names)].filter(
    (name) => !knownEntries.has(name)
  )

  if (!missingNames.length) {
    return snapshot
  }

  const batches = batchMissingModelNames(missingNames)
  const freshEntries = new Map<string, ModelPricingEntry>()
  for (const batch of batches) {
    const freshConfig = await getModelPricing(batch)
    for (const entry of freshConfig.entries) {
      freshEntries.set(entry.model_name, entry)
    }
  }

  const rawPrevious = pricingValuesByModel(snapshot.options)
  const missingEntryList: ModelPricingEntry[] = []

  for (const name of missingNames) {
    const entry = freshEntries.get(name)
    if (!entry) {
      throw new Error(t('Failed to load model pricing'))
    }
    missingEntryList.push(entry)
    const targetSharedName = entry.numeric_model_name ?? entry.model_name

    const rawSharedValues = rawPrevious.get(targetSharedName) ?? {}
    for (const key of SHARED_PRICING_KEYS) {
      const newlyRead = entry.configured[key]
      const oldRaw = rawSharedValues[key]
      if (newlyRead !== oldRaw) {
        throw new Error(t('Reload pricing'))
      }
    }

    const rawExactValues = rawPrevious.get(entry.model_name) ?? {}
    for (const key of EXACT_PRICING_KEYS) {
      const newlyRead = entry.configured[key]
      const oldRaw = rawExactValues[key]
      if (newlyRead !== oldRaw) {
        throw new Error(t('Reload pricing'))
      }
    }

    if (
      !pluginExpressionsEqual(
        entry.configured['billing_setting.plugin_billing_expr'],
        rawExactValues['billing_setting.plugin_billing_expr']
      )
    ) {
      throw new Error(t('Reload pricing'))
    }
  }

  return {
    ...snapshot,
    entries: [...snapshot.entries, ...missingEntryList],
  }
}

export async function preparePricingChanges(
  snapshot: ModelPricingConfig,
  before: PricingOptions,
  after: PricingOptions,
  selectedModelNames?: Iterable<string>
): Promise<ModelPricingChange[]> {
  const previous = pricingValuesByModel(before)
  const next = pricingValuesByModel(after)
  const knownEntries = new Map(
    snapshot.entries.map((entry) => [entry.model_name, entry])
  )

  const allNames = new Set([...previous.keys(), ...next.keys()])
  const missingNames: string[] = []
  for (const name of allNames) {
    const oldValues = previous.get(name) ?? {}
    const newValues = next.get(name) ?? {}
    const isDirty = PRICING_KEYS.some((key) =>
      key === 'billing_setting.plugin_billing_expr'
        ? !pluginExpressionsEqual(oldValues[key], newValues[key])
        : oldValues[key] !== newValues[key]
    )
    if (isDirty && !knownEntries.has(name)) {
      missingNames.push(name)
    }
  }

  const resolved = await resolvePricingSnapshot(snapshot, missingNames)
  return buildPricingChanges(resolved, before, after, selectedModelNames)
}
