import type { ReactNode } from "react";
import { Modal } from "./ui";

interface Props {
  message: string;
  confirm: string;
  band?: ReactNode; // a HazardBand when the thing is in production
  onYes(): void;
  onNo(): void;
}

/** Confirm asks before a destructive step (the webview has no native confirm()). */
export function Confirm({ message, confirm, band, onYes, onNo }: Props) {
  return <Modal title={message} band={band} ok={confirm} danger onOk={onYes} onCancel={onNo} />;
}
