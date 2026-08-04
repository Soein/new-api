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

import {
  MATCH_EQ,
  SOURCE_PARAM,
  buildRequestRuleExpr,
  combineBillingExpr,
  parseTiersFromExpr,
  splitBillingExprAndRequestRules,
  tryParseRequestRuleExpr,
  type RequestRuleGroup,
} from '../billing-expr'
import { getDynamicPricingSummary } from '../dynamic-price'
import { evalExprLocally } from '../tier-expr'

const imageRules: RequestRuleGroup[] = [
  {
    name: 'quality_high',
    conditions: [
      {
        source: SOURCE_PARAM,
        path: 'quality',
        mode: MATCH_EQ,
        value: 'high',
      },
    ],
    multiplier: '2',
  },
]

describe('v2 per-image billing expressions', () => {
  test('combines a per-image base and traceable rule under one v2 prefix', () => {
    const ruleExpr = buildRequestRuleExpr(imageRules)
    const combined = combineBillingExpr(
      'v2:tier("base", per_image(0.04))',
      ruleExpr
    )

    assert.equal(
      combined,
      'v2:(tier("base", per_image(0.04))) * rule("quality_high", param("quality") == "high", 2)'
    )
    assert.deepEqual(splitBillingExprAndRequestRules(combined), {
      billingExpr: 'v2:tier("base", per_image(0.04))',
      requestRuleExpr: 'rule("quality_high", param("quality") == "high", 2)',
    })
    assert.deepEqual(tryParseRequestRuleExpr(ruleExpr), imageRules)
  })

  test('keeps legacy ternary rules editable while naming newly generated rules', () => {
    const legacy = '(param("quality") == "high" ? 2 : 1)'
    const parsed = tryParseRequestRuleExpr(legacy)

    assert.deepEqual(parsed, [
      {
        name: '',
        conditions: [
          {
            source: 'param',
            path: 'quality',
            mode: 'eq',
            value: 'high',
          },
        ],
        multiplier: '2',
      },
    ])
    assert.equal(
      buildRequestRuleExpr(parsed || []),
      'rule("rule_1", param("quality") == "high", 2)'
    )
  })

  test('ignores parentheses and operators inside request rule strings', () => {
    const rules: RequestRuleGroup[] = [
      {
        name: 'wide (landscape',
        conditions: [
          {
            source: SOURCE_PARAM,
            path: 'render(mode',
            mode: MATCH_EQ,
            value: 'wide && vivid)',
          },
          {
            source: SOURCE_PARAM,
            path: 'quality',
            mode: MATCH_EQ,
            value: 'high',
          },
        ],
        multiplier: '1.5',
      },
      imageRules[0],
    ]
    const ruleExpr = buildRequestRuleExpr(rules)
    const combined = combineBillingExpr(
      'v2:tier("base", per_image(0.04))',
      ruleExpr
    )

    assert.deepEqual(tryParseRequestRuleExpr(ruleExpr), rules)
    assert.deepEqual(splitBillingExprAndRequestRules(combined), {
      billingExpr: 'v2:tier("base", per_image(0.04))',
      requestRuleExpr: ruleExpr,
    })
  })

  test('parses and formats the fixed price as dollars per image', () => {
    assert.deepEqual(parseTiersFromExpr('v2:per_image(0.04)'), [
      { label: 'base', conditions: [], perImagePrice: 0.04 },
    ])

    const summary = getDynamicPricingSummary(
      {
        id: 1,
        model_name: 'image-model',
        quota_type: 0,
        model_ratio: 0,
        completion_ratio: 0,
        enable_groups: [],
        billing_mode: 'tiered_expr',
        billing_expr: 'v2:per_image(0.04)',
      },
      { tokenUnit: 'M' }
    )

    assert.equal(summary?.primaryEntries[0]?.formatted, '$0.04')
    assert.equal(summary?.primaryEntries[0]?.unit, 'image')
  })

  test('parses per-image pricing when it is combined with token pricing', () => {
    const tiers = parseTiersFromExpr(
      'v2:tier("base", p * 2 + c * 8 + per_image(0.04))'
    )

    assert.equal(tiers[0]?.inputPrice, 2)
    assert.equal(tiers[0]?.outputPrice, 8)
    assert.equal(tiers[0]?.perImagePrice, 0.04)
  })

  test('estimator multiplies the fixed price by trusted image count semantics', () => {
    const result = evalExprLocally('v2:per_image(0.04)', 0, 0, {
      cacheReadTokens: 0,
      cacheCreateTokens: 0,
      cacheCreate1hTokens: 0,
      imageTokens: 0,
      imageOutputTokens: 0,
      audioInputTokens: 0,
      audioOutputTokens: 0,
      imageCount: 3,
    })

    assert.equal(result.error, null)
    assert.equal(result.cost, 120_000)
  })
})
