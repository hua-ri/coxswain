interface Props {
  message: string;
  confirm: string;
  onYes(): void;
  onNo(): void;
}

/** Confirm is an in-page dialog (the webview has no native confirm()). */
export function Confirm({ message, confirm, onYes, onNo }: Props) {
  return (
    <div className="overlay" onClick={onNo}>
      <div className="dialog" onClick={(e) => e.stopPropagation()}>
        <p>{message}</p>
        <div className="buttons">
          <button onClick={onNo} autoFocus>
            Cancel
          </button>
          <button className="danger" onClick={onYes}>
            {confirm}
          </button>
        </div>
      </div>
    </div>
  );
}
