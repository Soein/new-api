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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { Field, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from '@/components/ui/popover'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { getCurrencyDisplay, getCurrencyLabel } from '@/lib/currency'
import { formatNumber, quotaUnitsFromDisplayAmount } from '@/lib/format'

import { normalizeQuotaComparisonValue } from '../constants'
import type { QuotaComparisonOperator } from '../types'

const OPERATOR_SYMBOLS: Record<QuotaComparisonOperator, string> = {
  lt: '<',
  lte: '≤',
  eq: '=',
  gte: '≥',
  gt: '>',
}

type UsersQuotaFilterProps = {
  operator?: QuotaComparisonOperator
  amount?: number
  onApply: (operator: QuotaComparisonOperator, amount: number) => void
  onClear: () => void
}

export function UsersQuotaFilter(props: UsersQuotaFilterProps) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [draftOperator, setDraftOperator] =
    useState<QuotaComparisonOperator>('lt')
  const [draftAmount, setDraftAmount] = useState('0')
  const hasFilter = props.operator !== undefined && props.amount !== undefined
  const parsedAmount = Number(draftAmount)
  const amountIsValid =
    draftAmount.trim() !== '' && Number.isFinite(parsedAmount)
  const normalizedQuotaValue = amountIsValid
    ? normalizeQuotaComparisonValue(
        draftOperator,
        quotaUnitsFromDisplayAmount(parsedAmount)
      )
    : null
  const comparisonIsValid = amountIsValid && normalizedQuotaValue !== null
  const currencyLabel = getCurrencyLabel()
  const { meta: currencyMeta } = getCurrencyDisplay()

  const operatorItems = [
    { value: 'lt', label: t('Less than') },
    { value: 'lte', label: t('Less than or equal') },
    { value: 'eq', label: t('Equals') },
    { value: 'gte', label: t('Greater than or equal') },
    { value: 'gt', label: t('Greater than') },
  ]

  const handleOpenChange = (nextOpen: boolean) => {
    if (nextOpen) {
      setDraftOperator(props.operator ?? 'lt')
      setDraftAmount(props.amount !== undefined ? String(props.amount) : '0')
    }
    setOpen(nextOpen)
  }

  const handleApply = () => {
    if (!comparisonIsValid) return
    props.onApply(draftOperator, parsedAmount)
    setOpen(false)
  }

  let label = t('Quota')
  if (props.operator !== undefined && props.amount !== undefined) {
    label = `${t('Quota')} ${OPERATOR_SYMBOLS[props.operator]} ${formatNumber(props.amount)}`
  }

  return (
    <Popover open={open} onOpenChange={handleOpenChange}>
      <PopoverTrigger
        render={
          <Button
            type='button'
            variant='outline'
            aria-label={label}
            className='border-dashed'
          />
        }
      >
        {label}
      </PopoverTrigger>
      <PopoverContent align='start' className='w-80 p-4'>
        <FieldGroup className='gap-3'>
          <Field>
            <FieldLabel htmlFor='user-quota-filter-operator'>
              {t('Comparison')}
            </FieldLabel>
            <Select
              items={operatorItems}
              value={draftOperator}
              onValueChange={(value) => {
                if (value !== null) {
                  setDraftOperator(value as QuotaComparisonOperator)
                }
              }}
            >
              <SelectTrigger id='user-quota-filter-operator'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent alignItemWithTrigger={false}>
                <SelectGroup>
                  {operatorItems.map((item) => (
                    <SelectItem key={item.value} value={item.value}>
                      {item.label}
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>
          </Field>

          <Field data-invalid={!comparisonIsValid}>
            <FieldLabel htmlFor='user-quota-filter-amount'>
              {t('Quota ({{currency}})', { currency: currencyLabel })}
            </FieldLabel>
            <Input
              id='user-quota-filter-amount'
              type='number'
              step={currencyMeta.kind === 'tokens' ? '1' : 'any'}
              inputMode='decimal'
              value={draftAmount}
              aria-invalid={!comparisonIsValid}
              placeholder={t('Enter amount in {{currency}}', {
                currency: currencyLabel,
              })}
              onChange={(event) => setDraftAmount(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter') handleApply()
              }}
            />
          </Field>

          <div className='flex justify-end gap-2'>
            {hasFilter && (
              <Button
                type='button'
                variant='ghost'
                onClick={() => {
                  props.onClear()
                  setOpen(false)
                }}
              >
                {t('Clear')}
              </Button>
            )}
            <Button
              type='button'
              onClick={handleApply}
              disabled={!comparisonIsValid}
            >
              {t('Apply Filters')}
            </Button>
          </div>
        </FieldGroup>
      </PopoverContent>
    </Popover>
  )
}
