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
import { Delete02Icon } from '@hugeicons/core-free-icons'
import { HugeiconsIcon } from '@hugeicons/react'
import type { Table } from '@tanstack/react-table'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { DataTableBulkActions as BulkActionsToolbar } from '@/components/data-table'
import { Button } from '@/components/ui/button'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'

import { batchDeleteUsers } from '../api'
import type { User, UserDeletionTarget } from '../types'
import { useUsers } from './users-provider'

interface DataTableBulkActionsProps {
  table: Table<User>
  isUnavailable: boolean
}

export function DataTableBulkActions({
  table,
  isUnavailable,
}: DataTableBulkActionsProps) {
  const { t } = useTranslation()
  const { triggerRefresh } = useUsers()
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false)
  const [pendingUsers, setPendingUsers] = useState<UserDeletionTarget[]>([])
  const [isDeleting, setIsDeleting] = useState(false)
  const selectedUsers = table.getFilteredSelectedRowModel().rows.map((row) => ({
    id: row.original.id,
    identity_generation: row.original.identity_generation,
  }))

  const handleDelete = async () => {
    if (pendingUsers.length === 0 || isUnavailable) return

    setIsDeleting(true)
    try {
      const result = await batchDeleteUsers(pendingUsers)
      if (!result.success) {
        toast.error(result.message || t('Failed to delete selected users'))
        return
      }

      toast.success(
        t('{{count}} user(s) deleted', {
          count: result.data ?? pendingUsers.length,
        })
      )
      table.resetRowSelection()
      triggerRefresh()
      setShowDeleteConfirm(false)
      setPendingUsers([])
    } catch {
      toast.error(t('Failed to delete selected users'))
    } finally {
      setIsDeleting(false)
    }
  }

  return (
    <>
      <BulkActionsToolbar table={table} entityName='user'>
        <Tooltip>
          <TooltipTrigger
            render={
              <Button
                variant='destructive'
                size='icon'
                className='size-8'
                disabled={isUnavailable || selectedUsers.length === 0}
                onClick={() => {
                  setPendingUsers(selectedUsers)
                  setShowDeleteConfirm(true)
                }}
                aria-label={t('Delete selected users')}
                title={t('Delete selected users')}
              />
            }
          >
            <HugeiconsIcon icon={Delete02Icon} data-icon='inline-start' />
            <span className='sr-only'>{t('Delete selected users')}</span>
          </TooltipTrigger>
          <TooltipContent>
            <p>{t('Delete selected users')}</p>
          </TooltipContent>
        </Tooltip>
      </BulkActionsToolbar>

      <ConfirmDialog
        open={showDeleteConfirm}
        onOpenChange={(open) => {
          if (isDeleting) return
          setShowDeleteConfirm(open)
          if (!open) setPendingUsers([])
        }}
        title={t('Delete selected users?')}
        desc={t(
          'This will permanently delete {{count}} selected user(s). This action cannot be undone.',
          { count: pendingUsers.length }
        )}
        confirmText={isDeleting ? t('Deleting...') : t('Delete')}
        destructive
        isLoading={isDeleting}
        disabled={isUnavailable}
        handleConfirm={handleDelete}
      />
    </>
  )
}
