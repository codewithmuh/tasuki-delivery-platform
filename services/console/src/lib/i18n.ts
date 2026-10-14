/**
 * English / Japanese strings for the customer tracking page: a typed dictionary, no framework.
 * `ja` must define every key in `en` (the compiler checks), so a missing translation is a build
 * error, not a blank label.
 */
export type Lang = 'en' | 'ja';
export const LANGS: readonly Lang[] = ['en', 'ja'];

const en = {
  page_title: 'Track your delivery',
  headline_waiting: 'Getting the latest status…',
  headline_confirmed: 'Order confirmed. Finding a courier.',
  headline_assigned: 'A courier is heading to the store.',
  headline_picked_up: 'Your order is on its way.',
  headline_delivered: 'Delivered. Enjoy!',
  headline_cancelled: 'This delivery was cancelled.',
  headline_failed: 'We could not complete this delivery.',
  headline_expired: 'This tracking link has expired.',
  link_invalid: 'This tracking link has expired or is not valid.',
  link_invalid_hint:
    'Tracking links stop working 24 hours after delivery. Please contact the shop if you need help.',
  step_confirmed: 'Order confirmed',
  step_assigned: 'Courier assigned',
  step_picked_up: 'Picked up',
  step_delivered: 'Delivered',
  step_done: 'done',
  step_current: 'in progress',
  progress_label: 'Delivery progress',
  eta: 'Arriving around {time} (in about {n} min)',
  eta_due: 'Arriving any minute',
  updated_ago: 'Updated {age} ago',
  updated_never: 'Waiting for the first update',
  location_stale: 'The courier’s location may be out of date.',
  age_seconds: '{n} s',
  age_minutes: '{n} min',
  conn_connecting: 'Connecting…',
  conn_open: 'Live',
  conn_reconnecting: 'Reconnecting in {n} s',
  conn_closed: 'Not connected',
  switch_language: '日本語',
  switch_language_label: 'Switch to Japanese',
  map_label: 'Map with your courier’s current location',
  map_waiting: 'The courier’s location appears once they are on their way.',
  demo_note: 'Demo: Tasuki is a fictional company and all data is simulated.',
} as const;

export type MessageKey = keyof typeof en;

const ja: Record<MessageKey, string> = {
  page_title: '配達状況',
  headline_waiting: '最新の状況を取得しています…',
  headline_confirmed: 'ご注文を承りました。配達員を探しています。',
  headline_assigned: '配達員がお店に向かっています。',
  headline_picked_up: '配達員がお届け先に向かっています。',
  headline_delivered: 'お届けしました。',
  headline_cancelled: 'この配達はキャンセルされました。',
  headline_failed: '配達を完了できませんでした。',
  headline_expired: 'この追跡リンクは有効期限が切れています。',
  link_invalid: 'この追跡リンクは有効期限が切れているか、無効です。',
  link_invalid_hint:
    '追跡リンクはお届けの24時間後に無効になります。ご不明な点はご注文のお店にお問い合わせください。',
  step_confirmed: 'ご注文確定',
  step_assigned: '配達員決定',
  step_picked_up: '商品受け取り',
  step_delivered: 'お届け完了',
  step_done: '完了',
  step_current: '進行中',
  progress_label: '配達の進捗',
  eta: '{time}頃にお届け予定（あと約{n}分）',
  eta_due: 'まもなくお届けします',
  updated_ago: '{age}前に更新',
  updated_never: '最初の更新を待っています',
  location_stale: '配達員の位置情報が古い可能性があります。',
  age_seconds: '{n}秒',
  age_minutes: '{n}分',
  conn_connecting: '接続しています…',
  conn_open: 'ライブ',
  conn_reconnecting: '{n}秒後に再接続します',
  conn_closed: '接続されていません',
  switch_language: 'English',
  switch_language_label: '英語に切り替える',
  map_label: '配達員の現在地を示す地図',
  map_waiting: '配達員が出発すると、ここに現在地が表示されます。',
  demo_note: 'デモ：Tasukiは架空の会社で、表示されるデータはすべてシミュレーションです。',
};

const dictionaries: Record<Lang, Record<MessageKey, string>> = { en, ja };

/** Look up a message and fill `{name}` placeholders. Falls back to English, then to the key. */
export function t(lang: Lang, key: MessageKey, vars: Record<string, string | number> = {}): string {
  const template = dictionaries[lang][key] || en[key] || key;
  return template.replace(/\{(\w+)\}/g, (match, name: string) =>
    name in vars ? String(vars[name]) : match,
  );
}

/** A saved choice wins; otherwise Japanese if the browser prefers it; otherwise English. */
export function detectLang(saved: string | null, browserLanguages: readonly string[]): Lang {
  if (saved === 'en' || saved === 'ja') return saved;
  return browserLanguages.some((l) => l.toLowerCase().startsWith('ja')) ? 'ja' : 'en';
}

/** "12 s" / "3 min" (or "12秒" / "3分"). */
export function formatAge(lang: Lang, ageMs: number): string {
  const seconds = Math.max(0, Math.floor(ageMs / 1000));
  if (seconds < 120) return t(lang, 'age_seconds', { n: seconds });
  return t(lang, 'age_minutes', { n: Math.floor(seconds / 60) });
}

/** Clock time in Tokyo, whatever the viewer's own time zone: the delivery happens in Tokyo. */
export function formatTokyoTime(lang: Lang, epochMs: number): string {
  return new Intl.DateTimeFormat(lang === 'ja' ? 'ja-JP' : 'en-GB', {
    hour: '2-digit',
    minute: '2-digit',
    timeZone: 'Asia/Tokyo',
  }).format(epochMs);
}
