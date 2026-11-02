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
import { flattenBinary } from '@/features/pricing/lib/billing-expression/display'
import { compileBillingExpression } from '@/features/pricing/lib/billing-expression/parser'
import {
  expressionDependencies,
  type ExpressionNode,
  TOKEN_VARIABLES,
} from '@/features/pricing/lib/billing-expression/types'
import {
  parseVisualBillingDocument,
  serializeVisualBillingDocument,
} from '@/features/pricing/lib/billing-expression/visual'

import type { PricingSyncValues } from '../types'

export type PriceAdjustment = 'keep' | 'divide' | 'multiply'
export type AdjustedSyncPrice =
  | { values: PricingSyncValues; error?: never }
  | { values?: never; error: string }

// Always derive from the immutable upstream quote, never from a previous result.
export function adjustSyncPrice(
  values: PricingSyncValues,
  mode: PriceAdjustment,
  rate: number | null
): AdjustedSyncPrice {
  if (mode === 'keep') return { values }
  if (rate === null || !Number.isFinite(rate) || rate <= 0) {
    return { error: 'A valid site exchange rate is required.' }
  }
  const adjusted = { ...values }
  if (values.billing_mode !== 'tiered_expr') {
    const field =
      typeof values.model_price === 'number' ? 'model_price' : 'model_ratio'
    const amount = values[field]
    if (typeof amount !== 'number') return { error: 'Unset price' }
    const price = adjustPriceAmount(amount, mode, rate)
    if (price === null) {
      return { error: 'The adjusted price is outside the supported range.' }
    }
    adjusted[field] = price
    return { values: adjusted }
  }
  const source = values.billing_expr
  if (typeof source !== 'string') return { error: 'Unset price' }
  const compiled = compileBillingExpression(source)
  const unsupported = {
    error:
      'This expression cannot be adjusted automatically. Keep the source price or edit it manually.',
  }
  if (compiled.status !== 'ready') return unsupported
  const stack: ExpressionNode[] = [compiled.ast]
  const patches: { start: number; end: number; text: string }[] = []
  while (stack.length) {
    const node = stack.pop()
    if (!node) break
    if (node.kind === 'conditional') {
      const calls = expressionDependencies(node.condition).functions
      if (calls.has('tier') || calls.has('fixed')) return unsupported
      stack.push(node.yes, node.no)
      continue
    }
    if (node.kind === 'binary' && node.operator === '*') {
      const factor = [node.left, node.right].find(
        (candidate) =>
          (candidate.kind === 'variable' && candidate.name === 'image_count') ||
          compiled.requestRules.some((rule) => rule.node === candidate)
      )
      if (!factor) return unsupported
      stack.push(factor === node.left ? node.right : node.left)
      continue
    }
    if (node.kind !== 'call' || node.name !== 'tier') return unsupported
    // The shared visual adapter validates the entire price leaf. It preserves
    // zero fields and source spans; arbitrary/nonlinear bodies are not guessed.
    const document = parseVisualBillingDocument(
      source.slice(node.start, node.end)
    )
    if (document && document.root.kind === 'tier') {
      const tier = document.root
      if (tier.billingUnit === 'request') {
        const price = adjustPriceAmount(Number(tier.fixedPrice), mode, rate)
        if (price === null) {
          return { error: 'The adjusted price is outside the supported range.' }
        }
        tier.fixedPrice = String(price)
      } else {
        for (const entry of tier.prices) {
          const price = adjustPriceAmount(Number(entry.value), mode, rate)
          if (price === null) {
            return {
              error: 'The adjusted price is outside the supported range.',
            }
          }
          entry.value = String(price)
        }
      }
      const serialized = serializeVisualBillingDocument(document)
      if (!serialized.ok) {
        return { error: 'The adjusted price is outside the supported range.' }
      }
      patches.push({
        start: node.start,
        end: node.end,
        text: serialized.source,
      })
      continue
    }

    const leaves = extractTierPriceLeaves(node, compiled.version)
    if (!leaves || leaves.length === 0) return unsupported
    for (const leaf of leaves) {
      const price = adjustPriceAmount(Number(leaf.value), mode, rate)
      if (price === null) {
        return { error: 'The adjusted price is outside the supported range.' }
      }
      patches.push({ start: leaf.start, end: leaf.end, text: String(price) })
    }
  }
  let expression = source
  for (const patch of patches.sort((a, b) => b.start - a.start)) {
    expression =
      expression.slice(0, patch.start) +
      patch.text +
      expression.slice(patch.end)
  }
  if (compileBillingExpression(expression).status !== 'ready') {
    return unsupported
  }
  adjusted.billing_expr = expression
  return { values: adjusted }
}

function adjustPriceAmount(
  amount: number,
  mode: PriceAdjustment,
  rate: number
): number | null {
  const price = mode === 'divide' ? amount / rate : amount * rate
  if (
    !Number.isFinite(amount) ||
    amount < 0 ||
    !Number.isFinite(price) ||
    price < 0 ||
    (amount > 0 && price === 0)
  ) {
    return null
  }
  return price
}

function extractTierPriceLeaves(
  node: ExpressionNode,
  version: number | string | undefined
): (ExpressionNode & { kind: 'literal'; value: number })[] | null {
  if (
    node.kind !== 'call' ||
    node.name !== 'tier' ||
    node.args.length !== 2 ||
    node.args[0].kind !== 'literal' ||
    typeof node.args[0].value !== 'string'
  ) {
    return null
  }
  const terms = flattenBinary(node.args[1], '+')
  if (terms.length === 0) return null
  const leaves: (ExpressionNode & { kind: 'literal'; value: number })[] = []
  const seenVars = new Set<string>()
  let hasPerImage = false

  for (const term of terms) {
    if (term.kind === 'call' && term.name === 'per_image') {
      if (
        (version !== 2 && version !== 'v2') ||
        hasPerImage ||
        term.args.length !== 1 ||
        term.args[0].kind !== 'literal' ||
        typeof term.args[0].value !== 'number' ||
        !Number.isFinite(term.args[0].value) ||
        term.args[0].value < 0
      ) {
        return null
      }
      hasPerImage = true
      leaves.push(
        term.args[0] as ExpressionNode & { kind: 'literal'; value: number }
      )
      continue
    }
    if (term.kind === 'call' && term.name === 'fixed') {
      if (
        terms.length !== 1 ||
        term.args.length !== 1 ||
        term.args[0].kind !== 'literal' ||
        typeof term.args[0].value !== 'number' ||
        !Number.isFinite(term.args[0].value) ||
        term.args[0].value < 0
      ) {
        return null
      }
      leaves.push(
        term.args[0] as ExpressionNode & { kind: 'literal'; value: number }
      )
      continue
    }
    if (term.kind === 'binary' && term.operator === '*') {
      let variableNode: ExpressionNode | null = null
      let priceNode:
        | (ExpressionNode & { kind: 'literal'; value: number })
        | null = null
      if (
        term.left.kind === 'variable' &&
        (TOKEN_VARIABLES as readonly string[]).includes(term.left.name) &&
        term.left.name !== 'len' &&
        term.left.name !== 'image_count' &&
        term.right.kind === 'literal' &&
        typeof term.right.value === 'number' &&
        Number.isFinite(term.right.value) &&
        term.right.value >= 0
      ) {
        variableNode = term.left
        priceNode = term.right as ExpressionNode & {
          kind: 'literal'
          value: number
        }
      } else if (
        term.right.kind === 'variable' &&
        (TOKEN_VARIABLES as readonly string[]).includes(term.right.name) &&
        term.right.name !== 'len' &&
        term.right.name !== 'image_count' &&
        term.left.kind === 'literal' &&
        typeof term.left.value === 'number' &&
        Number.isFinite(term.left.value) &&
        term.left.value >= 0
      ) {
        variableNode = term.right
        priceNode = term.left as ExpressionNode & {
          kind: 'literal'
          value: number
        }
      }
      if (!variableNode || !priceNode || seenVars.has(variableNode.name)) {
        return null
      }
      seenVars.add(variableNode.name)
      leaves.push(priceNode)
      continue
    }
    return null
  }
  return leaves.length > 0 ? leaves : null
}
