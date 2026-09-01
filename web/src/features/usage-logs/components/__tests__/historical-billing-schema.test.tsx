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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import i18next from 'i18next'
import { afterEach, beforeAll, describe, expect, test } from 'vitest'

import type { PricingModel } from '@/features/pricing/types'

import type { UsageLog } from '../../data/schema'
import type { LogOtherData } from '../../types'
import { DetailsDialog } from '../dialogs/details-dialog'

const i18nKeys = {
  'Log Details': 'Log Details',
  Consume: 'Consume',
  'Billing Details': 'Billing Details',
  'Billing Mode': 'Billing Mode',
  'Dynamic Pricing': 'Dynamic Pricing',
  'Matched Tier': 'Matched Tier',
  'Total Cost': 'Total Cost',
  'Tiered price table': 'Tiered price table',
  s: 's',
  unit: 'unit',
  request: 'request',
}

function makeLog(other: LogOtherData): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: 1,
    type: 2,
    content: '',
    username: 'user',
    token_name: 'token',
    model_name: 'video-gen-v1',
    quota: 5000,
    prompt_tokens: 0,
    completion_tokens: 0,
    use_time: 0,
    is_stream: false,
    channel: 1,
    channel_name: '',
    token_id: 1,
    group: 'default',
    ip: '',
    other: JSON.stringify(other),
    request_id: 'req-1',
    upstream_request_id: '',
  }
}

function renderDetails(
  other: LogOtherData,
  currentModels: PricingModel[]
): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const freshAt = Date.now() + 60_000
  queryClient.setQueryData(['status'], {}, { updatedAt: freshAt })
  queryClient.setQueryData(
    ['pricing'],
    { data: currentModels, vendors: [] },
    { updatedAt: freshAt }
  )

  render(
    <QueryClientProvider client={queryClient}>
      <DetailsDialog
        log={makeLog(other)}
        isAdmin={false}
        isRoot={false}
        open
        onOpenChange={() => undefined}
      />
    </QueryClientProvider>
  )
  return queryClient
}

describe('historical billing usage schema selection', () => {
  const queryClients: QueryClient[] = []

  beforeAll(() => {
    i18next.addResourceBundle('en', 'translation', i18nKeys)
  })

  afterEach(() => {
    for (const queryClient of queryClients) {
      queryClient.clear()
    }
    queryClients.length = 0
  })

  test('prefers other.billing_usage_schema snapshot over current pricing schema', () => {
    const expression = 'tier("base", u("duration") * 0.005)'
    const exprB64 = Buffer.from(expression, 'utf8').toString('base64')

    const currentPricingModels: PricingModel[] = [
      {
        id: 1,
        model_name: 'video-gen-v1',
        quota_type: 1,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: expression,
        billing_usage_schema: {
          duration: { type: 'number', unit: 'count' }, // Current pricing uses "count"
        },
      },
    ]

    // Historical log has snapshot with unit "second"
    queryClients.push(
      renderDetails(
        {
          group_ratio: 1,
          billing_mode: 'tiered_expr',
          expr_b64: exprB64,
          matched_tier: 'base',
          billing_usage_schema: {
            duration: { type: 'number', unit: 'second' },
          },
          usage_facts: { duration: 10 },
        },
        currentPricingModels
      )
    )

    // Should render with historical unit '/s', not current '/unit'
    expect(screen.getAllByText(/\$0\.0050\/s/).length).toBeGreaterThan(0)
    expect(screen.queryByText(/\$0\.0050\/unit/)).toBeNull()
  })

  test('falls back to current pricing schema when log lacks billing_usage_schema snapshot', () => {
    const expression = 'tier("base", u("duration") * 0.005)'
    const exprB64 = Buffer.from(expression, 'utf8').toString('base64')

    const currentPricingModels: PricingModel[] = [
      {
        id: 1,
        model_name: 'video-gen-v1',
        quota_type: 1,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: expression,
        billing_usage_schema: {
          duration: { type: 'number', unit: 'count' }, // Current pricing uses "count"
        },
      },
    ]

    // Legacy log does not have billing_usage_schema
    queryClients.push(
      renderDetails(
        {
          group_ratio: 1,
          billing_mode: 'tiered_expr',
          expr_b64: exprB64,
          matched_tier: 'base',
          usage_facts: { duration: 10 },
        },
        currentPricingModels
      )
    )

    // Should fall back to current pricing unit '/unit'
    expect(screen.getAllByText(/\$0\.0050\/unit/).length).toBeGreaterThan(0)
  })
})
