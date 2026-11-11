"use client"

import { useState } from "react"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { useObjectUrl } from "@/components/ui/file-thumbnail"

// An image sent in the Thread, full size.
export function ImageDialog({ file, onClose }: { file: File | undefined; onClose: () => void }) {
  // The image stays while the dialog closes.
  const [shown, setShown] = useState(file)
  if (file && file !== shown) setShown(file)
  const url = useObjectUrl(shown ?? null)
  return (
    <Dialog open={file !== undefined} onOpenChange={(open) => !open && onClose()}>
      <DialogContent size="xl" className="flex flex-col gap-3 p-3 pt-12">
        <DialogTitle className="absolute left-4 top-4 text-sm font-medium">{shown?.name}</DialogTitle>
        {url && (
          // eslint-disable-next-line @next/next/no-img-element -- an image the owner sent, held by the page
          <img src={url} alt={shown?.name ?? ""} className="max-h-[80dvh] w-full rounded-md object-contain" />
        )}
      </DialogContent>
    </Dialog>
  )
}
