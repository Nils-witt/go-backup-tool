import { useEffect, useRef, type ReactNode } from "react";

interface DialogProps {
  open: boolean;
  onClose: (returnValue: string) => void;
  className?: string;
  children: ReactNode;
}

// Dialog wraps the native <dialog> element (the same one dashboard.html
// uses for every confirm/info popup), driving showModal()/close() from a
// controlled `open` prop.
export function Dialog({ open, onClose, className, children }: DialogProps) {
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const dlg = ref.current;
    if (!dlg) return;

    if (open && !dlg.open) dlg.showModal();
    if (!open && dlg.open) dlg.close();
  }, [open]);

  return (
    <dialog
      ref={ref}
      className={className ?? "confirm-dialog"}
      onClose={(e) => onClose((e.target as HTMLDialogElement).returnValue)}
    >
      {children}
    </dialog>
  );
}

interface ConfirmDialogProps {
  open: boolean;
  message: ReactNode;
  confirmLabel: string;
  onConfirm: () => void;
  onCancel: () => void;
  children?: ReactNode;
}

// ConfirmDialog covers the six near-identical "<dialog class="confirm-dialog">"
// blocks in dashboard.html that ask "do this?" with Cancel/confirm buttons
// (retry, download, delete user, delete OIDC override, plus issue-token's
// extra "days" field via children).
export function ConfirmDialog({ open, message, confirmLabel, onConfirm, onCancel, children }: ConfirmDialogProps) {
  return (
    <Dialog
      open={open}
      onClose={(returnValue) => {
        if (returnValue === "confirm") onConfirm();
        else onCancel();
      }}
    >
      <form method="dialog">
        <p className="confirm-message">{message}</p>
        {children}
        <div className="confirm-actions">
          <button type="submit" value="cancel" className="btn btn-secondary">
            Cancel
          </button>
          <button type="submit" value="confirm" className="btn btn-primary" autoFocus>
            {confirmLabel}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
