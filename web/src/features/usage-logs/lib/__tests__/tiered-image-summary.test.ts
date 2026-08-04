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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { getTieredBillingSummary } from '../format'

describe('tiered image billing summary', () => {
  test('preserves per-image pricing as an image unit', () => {
    const expression =
      'v2:tier("base", per_image(0.04)) * rule("quality_high", param("quality") == "high", 2)'
    const summary = getTieredBillingSummary({
      billing_mode: 'tiered_expr',
      expr_b64: Buffer.from(expression).toString('base64'),
      matched_tier: 'base',
      image_count: 3,
    })

    assert.ok(summary)
    assert.deepEqual(summary.priceEntries, [
      {
        field: 'perImagePrice',
        shortLabel: 'Per image',
        price: 0.04,
        unit: 'image',
      },
    ])
  })
})
