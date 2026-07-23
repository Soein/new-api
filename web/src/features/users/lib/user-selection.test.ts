import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import type { User } from '../types'

const userConstants: Record<string, unknown> = await import('../constants.ts')

describe('bulk user deletion selection', () => {
  test('only allows targets below the current administrator role', () => {
    const canBulkDeleteUser = userConstants.canBulkDeleteUser
    assert.equal(typeof canBulkDeleteUser, 'function')
    if (typeof canBulkDeleteUser !== 'function') return

    const currentUser = { id: 1, role: 10 }
    const commonUser = { id: 2, role: 1, DeletedAt: null } as User
    const currentUserRow = { id: 1, role: 10, DeletedAt: null } as User
    const peerAdmin = { id: 3, role: 10, DeletedAt: null } as User
    const rootUser = { id: 4, role: 100, DeletedAt: null } as User
    const deletedUser = { id: 5, role: 1, DeletedAt: {} } as User

    assert.equal(canBulkDeleteUser(currentUser, commonUser), true)
    assert.equal(canBulkDeleteUser(currentUser, currentUserRow), false)
    assert.equal(canBulkDeleteUser(currentUser, peerAdmin), false)
    assert.equal(canBulkDeleteUser(currentUser, rootUser), false)
    assert.equal(canBulkDeleteUser(currentUser, deletedUser), false)
    assert.equal(canBulkDeleteUser(null, commonUser), false)
  })

  test('normalizes fractional quota thresholds without changing comparison semantics', () => {
    const normalizeQuotaComparisonValue =
      userConstants.normalizeQuotaComparisonValue
    assert.equal(typeof normalizeQuotaComparisonValue, 'function')
    if (typeof normalizeQuotaComparisonValue !== 'function') return

    assert.equal(normalizeQuotaComparisonValue('lt', 0.5), 1)
    assert.equal(normalizeQuotaComparisonValue('lte', 0.5), 0)
    assert.equal(normalizeQuotaComparisonValue('gt', 0.5), 0)
    assert.equal(normalizeQuotaComparisonValue('gte', 0.5), 1)
    assert.equal(normalizeQuotaComparisonValue('eq', 0.5), null)
    assert.equal(normalizeQuotaComparisonValue('eq', 1), 1)
    assert.equal(normalizeQuotaComparisonValue('lt', -0.5), 0)
    assert.equal(normalizeQuotaComparisonValue('lte', -0.5), -1)
    assert.equal(normalizeQuotaComparisonValue('eq', Number.NaN), null)
    assert.equal(
      normalizeQuotaComparisonValue('lt', Number.POSITIVE_INFINITY),
      null
    )
    assert.equal(normalizeQuotaComparisonValue('lte', 2_147_483_648), null)
    assert.equal(normalizeQuotaComparisonValue('gte', -2_147_483_649), null)
  })
})
