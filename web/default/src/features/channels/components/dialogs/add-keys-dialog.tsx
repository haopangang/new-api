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
import { useQueryClient } from '@tanstack/react-query'
import { Loader2, Plus } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/dialog'
import { Textarea } from '@/components/ui/textarea'
import { Label } from '@/components/ui/label'
import { manageMultiKeys } from '../../api'
import { channelsQueryKeys } from '../../lib'

type AddKeysDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  channelId: number
  onSuccess: () => void
}

export function AddKeysDialog({
  open,
  onOpenChange,
  channelId,
  onSuccess,
}: AddKeysDialogProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [keysInput, setKeysInput] = useState('')
  const [isSubmitting, setIsSubmitting] = useState(false)

  const handleSubmit = async () => {
    if (!keysInput.trim()) {
      toast.error(t('Please enter keys to add'))
      return
    }

    setIsSubmitting(true)
    try {
      const response = await manageMultiKeys({
        channel_id: channelId,
        action: 'add_keys',
        keys: keysInput.trim(),
      })

      if (response.success) {
        toast.success(response.message || t('Keys added successfully'))
        queryClient.invalidateQueries({ queryKey: channelsQueryKeys.lists() })
        setKeysInput('')
        onSuccess()
        onOpenChange(false)
      } else {
        toast.error(response.message || t('Failed to add keys'))
      }
    } catch (error: unknown) {
      toast.error(
        error instanceof Error ? error.message : t('Failed to add keys')
      )
    } finally {
      setIsSubmitting(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('Add Keys')}
      description={t('Add new keys to this multi-key channel')}
      contentClassName='max-w-2xl'
    >
      <div className='space-y-4'>
        <div className='space-y-2'>
          <Label htmlFor='keys-input'>
            {t('Keys (one per line or JSON array)')}
          </Label>
          <Textarea
            id='keys-input'
            placeholder={t(
              'Enter keys, one per line, or paste a JSON array'
            )}
            value={keysInput}
            onChange={(e) => setKeysInput(e.target.value)}
            rows={10}
            className='font-mono text-sm'
          />
          <p className='text-muted-foreground text-sm'>
            {t('Each line will be treated as a separate key')}
          </p>
        </div>

        <div className='flex justify-end gap-2'>
          <Button
            variant='outline'
            onClick={() => onOpenChange(false)}
            disabled={isSubmitting}
          >
            {t('Cancel')}
          </Button>
          <Button onClick={handleSubmit} disabled={isSubmitting || !keysInput.trim()}>
            {isSubmitting ? (
              <Loader2 className='mr-2 h-4 w-4 animate-spin' />
            ) : (
              <Plus className='mr-2 h-4 w-4' />
            )}
            {t('Add Keys')}
          </Button>
        </div>
      </div>
    </Dialog>
  )
}
