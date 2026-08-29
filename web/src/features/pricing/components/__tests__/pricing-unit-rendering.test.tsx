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
import { render, screen, within } from '@testing-library/react'
import type * as React from 'react'
import { describe, expect, test, vi } from 'vitest'

import type { PricingModel } from '../../types'
import { DynamicPricingBreakdown } from '../dynamic-pricing-breakdown'
import { ModelCard } from '../model-card'
import { usePricingColumns } from '../pricing-columns'

vi.mock('@/lib/lobe-icon', () => ({
  getLobeIcon: () => null,
}))
vi.mock('@lobehub/fluent-emoji', () => ({
  FluentEmoji: () => null,
}))

function PricingCellTest({ model }: { model: PricingModel }) {
  const columns = usePricingColumns({
    tokenUnit: 'M',
    showRechargePrice: false,
    priceRate: 1,
    usdExchangeRate: 1,
    selectedGroup: 'default',
  })
  const priceCol = columns.find(
    (c) => 'accessorKey' in c && c.accessorKey === 'price'
  )
  if (!priceCol || typeof priceCol.cell !== 'function') return null
  const CellRenderer = priceCol.cell as (props: {
    row: { original: PricingModel }
  }) => React.ReactNode
  return <>{CellRenderer({ row: { original: model } })}</>
}

describe('pricing unit duplication and model card regression tests', () => {
  describe('pricing-columns unit branching', () => {
    test('single-unit image model displays /image in subtitle only, not duplicated in main price', () => {
      const model: PricingModel = {
        id: 1,
        model_name: 'flux-schnell',
        quota_type: 0,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: 'v2:tier("base", per_image(0.04))',
      }

      const { container } = render(<PricingCellTest model={model} />)
      const cellText = container.textContent || ''
      const matches = cellText.match(/image/gi) || []
      expect(matches).toHaveLength(1)
      expect(cellText).toMatch(/\$0\.04/)
      expect(cellText).toMatch(/\/\s*image/)
    })

    test('token dynamic model displays tokenUnitLabel in subtitle and no inlined token unit on main numbers', () => {
      const model: PricingModel = {
        id: 2,
        model_name: 'gpt-4o',
        quota_type: 0,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: 'tier("base", p * 2.5 + c * 10)',
      }

      const { container } = render(<PricingCellTest model={model} />)
      const cellText = container.textContent || ''
      expect(cellText).toMatch(/\$2\.5/)
      expect(cellText).toMatch(/\$10/)
      expect(cellText).toMatch(/\/\s*1M\s*tokens/)
    })

    test('task usage model inlines unit on main price and shows tier label in subtitle', () => {
      const model: PricingModel = {
        id: 3,
        model_name: 'kling-video',
        quota_type: 0,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: 'tier("std", u("seconds") * 0.4)',
        billing_usage_schema: {
          seconds: { type: 'number' as const, unit: 'second' as const },
        },
      }

      const { container } = render(<PricingCellTest model={model} />)
      const cellText = container.textContent || ''
      expect(cellText).toMatch(/\$0\.4/)
      expect(cellText).toMatch(/\/s/)
      expect(cellText).toMatch(/std/)
    })

    test('mixed unit model inlines unit on each entry', () => {
      const model: PricingModel = {
        id: 4,
        model_name: 'mixed-model',
        quota_type: 0,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: 'tier("base", p * 1.5 + per_image(0.02))',
      }

      const { container } = render(<PricingCellTest model={model} />)
      const cellText = container.textContent || ''
      expect(cellText).toMatch(/\$1\.5\/1M/)
      expect(cellText).toMatch(/\$0\.02\/image/)
    })
  })

  describe('dynamic-pricing-breakdown unit separation', () => {
    test('desktop table displays $/image in header and no duplicated /image in cell', () => {
      render(
        <DynamicPricingBreakdown billingExpr='v2:tier("base", per_image(0.04))' />
      )

      const table = screen.getByRole('table')
      const imageHeader = within(table).getByRole('columnheader', {
        name: /per image/i,
      })
      expect(imageHeader).toHaveTextContent('Per image ($/image)')

      const priceCell = within(table).getByRole('cell', { name: '$0.0400' })
      expect(priceCell).toBeInTheDocument()
      expect(priceCell.textContent).not.toContain('/image')
    })

    test('mobile card displays /image in price value while label retains clean field name', () => {
      render(
        <DynamicPricingBreakdown billingExpr='v2:tier("base", per_image(0.04))' />
      )

      // The table handles desktop layout with column headers
      const table = screen.getByRole('table')
      expect(table).toBeInTheDocument()

      // The mobile breakdown renders a clean label without ($/image) and a value bearing /image
      const mobileLabel = screen.getByText('Per image')
      expect(mobileLabel).toHaveTextContent(/^Per image$/)

      const mobileValue = screen.getByText('$0.0400/image')
      expect(mobileValue).toBeInTheDocument()
    })

    test('desktop table retains units for task fields (second, credit)', () => {
      render(
        <DynamicPricingBreakdown
          billingExpr='tier("base", u("seconds") * 0.4 + u("credits") * 0.1)'
          usageSchema={{
            seconds: { type: 'number', unit: 'second' },
            credits: { type: 'number', unit: 'credit' },
          }}
        />
      )

      const table = screen.getByRole('table')
      const cells = within(table).getAllByRole('cell')
      expect(cells.some((c) => c.textContent?.includes('$0.4000/s'))).toBe(true)
      expect(cells.some((c) => c.textContent?.includes('$0.1000/credit'))).toBe(
        true
      )
    })
  })

  describe('model-card token unit display', () => {
    test('pure image dynamic model card displays /image and does NOT display 1M', () => {
      const model: PricingModel = {
        id: 10,
        model_name: 'dall-e-3',
        quota_type: 0,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: 'v2:tier("base", per_image(0.04))',
      }

      render(
        <ModelCard
          model={model}
          tokenUnit='M'
          showRechargePrice={false}
          priceRate={1}
          usdExchangeRate={1}
          onClick={() => {}}
        />
      )

      expect(screen.getByText(/dall-e-3/)).toBeInTheDocument()
      expect(screen.getByText(/\$0\.04/)).toBeInTheDocument()
      expect(screen.getByText(/\/image/)).toBeInTheDocument()
      expect(screen.queryByText(/^1M$/)).toBeNull()
    })

    test('task usage token model card inlines /1M token and does NOT display bottom 1M tag', () => {
      const model: PricingModel = {
        id: 13,
        model_name: 'task-token-model',
        quota_type: 0,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: 'tier("base", u("tokens") * 9.8 / 1000000)',
        billing_usage_schema: {
          tokens: { type: 'number' as const, unit: 'token' as const },
        },
      }

      render(
        <ModelCard
          model={model}
          tokenUnit='M'
          showRechargePrice={false}
          priceRate={1}
          usdExchangeRate={1}
          onClick={() => {}}
        />
      )

      expect(screen.getByText(/task-token-model/)).toBeInTheDocument()
      expect(screen.getByText(/1M token/)).toBeInTheDocument()
      // Bottom token unit tag should not be present because price already inlines /1M token
      expect(screen.queryByText('1M')).toBeNull()
    })

    test('token dynamic model card displays 1M token unit', () => {
      const model: PricingModel = {
        id: 11,
        model_name: 'gpt-4o-card',
        quota_type: 0,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: 'tier("base", p * 2.5 + c * 10)',
      }

      render(
        <ModelCard
          model={model}
          tokenUnit='M'
          showRechargePrice={false}
          priceRate={1}
          usdExchangeRate={1}
          onClick={() => {}}
        />
      )

      expect(screen.getByText(/gpt-4o-card/)).toBeInTheDocument()
      expect(screen.getByText('1M')).toBeInTheDocument()
    })

    test('mixed dynamic model card displays /image and 1M token unit', () => {
      const model: PricingModel = {
        id: 12,
        model_name: 'mixed-card-model',
        quota_type: 0,
        model_ratio: 1,
        completion_ratio: 1,
        enable_groups: ['default'],
        billing_mode: 'tiered_expr',
        billing_expr: 'tier("base", p * 2.5 + per_image(0.04))',
      }

      render(
        <ModelCard
          model={model}
          tokenUnit='M'
          showRechargePrice={false}
          priceRate={1}
          usdExchangeRate={1}
          onClick={() => {}}
        />
      )

      expect(screen.getByText(/mixed-card-model/)).toBeInTheDocument()
      expect(screen.getByText(/\/image/)).toBeInTheDocument()
      expect(screen.getByText('1M')).toBeInTheDocument()
    })
  })
})
