/**
 * <date-range-input>: a free stand-in for Web Awesome Pro's
 * <wa-date-input mode="range">. The calendar is Cally (<calendar-range>), which the
 * host page loads separately; this module deliberately does not import it.
 * Multiple mode uses Cally's <calendar-multi>.
 *
 * Value format: "YYYY-MM-DD/YYYY-MM-DD" (start/end, inclusive); multiple mode
 * accepts space-separated start/end pairs.
 *
 * Marks (advisory, never disable a day): `marks="#id"` names an element in the same tree
 * holding JSON, normally <script type="application/json">:
 *   {"holidays": {"2026-12-25": "Christmas Day"},
 *    "booked": [{"start": "2026-12-14", "end": "2026-12-18", "status": "approved"}]}
 * Each such day's button gets the parts `holiday`, and `booked` plus `booked-<status>`,
 * exported as day-holiday, day-booked and day-booked-<status> for styling. It is read
 * each time the calendar opens, so a page may rewrite the block. Cally has no tooltip hook
 * for a day button, so the holiday name is not shown here.
 */

const CSS = /* css */ `
:host {
  display: block;
  font: inherit;
  color: inherit;
  --_background: var(--dri-background, light-dark(#fff, #18181b));
  --_color: var(--dri-color, light-dark(#18181b, #f4f4f5));
  --_border-color: var(--dri-border-color, light-dark(#d4d4d8, #3f3f46));
  --_border-radius: var(--dri-border-radius, 0.375rem);
  --_focus-ring: var(--dri-focus-ring, 2px solid light-dark(#2563eb, #60a5fa));
  --_accent: var(--dri-accent, light-dark(#18181b, #f4f4f5));
  --_accent-color: var(--dri-accent-color, light-dark(#fff, #18181b));
  --_range-background: var(--dri-range-background, light-dark(#e4e4e7, #3f3f46));
  --_muted: var(--dri-muted, light-dark(#71717a, #a1a1aa));
  --_invalid: var(--dri-invalid-color, light-dark(#dc2626, #f87171));
  --_popup-background: var(--dri-popup-background, light-dark(#fff, #232327));
  --_popup-shadow: var(--dri-popup-shadow, 0 8px 24px light-dark(rgb(0 0 0 / 0.15), rgb(0 0 0 / 0.55)));
  --_gap: var(--dri-popup-gap, 0.25rem);
  --_two-months: var(--dri-two-months-min-width, 36rem);
}
:host([hidden]) { display: none; }
[hidden] { display: none !important; }

.form-control { display: flex; flex-direction: column; gap: 0.375rem; }

label {
  font-weight: 500;
  color: var(--_color);
  cursor: default;
}
:host(:state(required)) label::after { content: " *" / ""; }

.hint { font-size: 0.875em; color: var(--_muted); }

.base {
  anchor-name: --dri-anchor;
  display: flex;
  align-items: center;
  box-sizing: border-box;
  min-block-size: 2.5rem;
  background: var(--_background);
  color: var(--_color);
  border: 1px solid var(--_border-color);
  border-radius: var(--_border-radius);
  &:has(.field:focus-visible), &:has(.clear:focus-visible) { outline: var(--_focus-ring); outline-offset: 2px; }
  &:has(.field[aria-expanded="true"]) { border-color: var(--_accent); }
}
:host(:state(user-invalid)) .base { border-color: var(--_invalid); }

.field {
  all: unset;
  box-sizing: border-box;
  flex: 1;
  min-inline-size: 0;
  align-self: stretch;
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding-inline: 0.75rem;
  cursor: pointer;
  font: inherit;
}
.value { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
:host(:state(blank)) .value { color: var(--_muted); }
.expand-icon { flex: none; inline-size: 1em; block-size: 1em; color: var(--_muted); }

.clear {
  all: unset;
  box-sizing: border-box;
  display: grid;
  place-items: center;
  inline-size: 1.75rem;
  block-size: 1.75rem;
  margin-inline-end: 0.25rem;
  border-radius: calc(var(--_border-radius) - 2px);
  color: var(--_muted);
  cursor: pointer;
  &:hover { color: var(--_color); background: var(--_range-background); }
  & svg { inline-size: 1em; block-size: 1em; }
}

:host(:state(disabled)) .base { opacity: 0.5; }
:host(:state(disabled)) .field, :host(:state(disabled)) label { cursor: not-allowed; }
:host([readonly]) .field { cursor: default; }

.vh {
  position: absolute; inline-size: 1px; block-size: 1px; overflow: hidden;
  clip-path: inset(50%); white-space: nowrap;
}

.popup {
  position-anchor: --dri-anchor;
  position-area: block-end span-inline-end;
  position-try-fallbacks: flip-block;
  inset: auto;
  margin: 0;
  margin-block: var(--_gap);
  box-sizing: border-box;
  max-inline-size: calc(100vw - 1rem);
  max-block-size: calc(100vh - 2rem);
  overflow: auto;
  padding: 0.75rem;
  background: var(--_popup-background);
  color: var(--_color);
  border: 1px solid var(--_border-color);
  border-radius: var(--_border-radius);
  box-shadow: var(--_popup-shadow);
  font: inherit;
}

.probe { position: absolute; visibility: hidden; pointer-events: none; block-size: 0; inline-size: var(--_two-months); }

.months { display: flex; gap: 1.5rem; }

calendar-range, calendar-multi {
  color: var(--_color);
  &::part(container) { position: relative; }
  &::part(heading) { display: none; }
  &::part(header) {
    position: absolute;
    inset-inline: 0;
    inset-block-start: 0;
    display: flex;
    justify-content: space-between;
    pointer-events: none;
  }
  &::part(button) {
    pointer-events: auto;
    display: grid;
    place-items: center;
    box-sizing: border-box;
    inline-size: 2.25rem;
    block-size: 2.25rem;
    padding: 0;
    border: 0;
    border-radius: var(--_border-radius);
    background: transparent;
    color: inherit;
    cursor: pointer;
  }
  &::part(button):hover { background-color: var(--_range-background); }
  &::part(button):focus-visible { outline: var(--_focus-ring); outline-offset: -2px; }
  &::part(disabled) { opacity: 0.35; cursor: default; }
  &::part(disabled):hover { background-color: transparent; }
  & svg { inline-size: 1em; block-size: 1em; }
}

calendar-month {
  --color-accent: var(--_accent);
  --color-text-on-accent: var(--_accent-color);
  color: var(--_color);
  &::part(heading) {
    display: grid;
    place-items: center;
    block-size: 2.25rem;
    font-weight: 600;
  }
  &::part(th) { color: var(--_muted); font-weight: 500; }
  &::part(button) { border-radius: var(--_border-radius); cursor: pointer; }
  &::part(button):hover { background-color: var(--_range-background); }
  &::part(today) { box-shadow: inset 0 0 0 1px var(--_muted); }
  &::part(selected) { background: var(--_accent); color: var(--_accent-color); }
  /* Keep the selected colours under the pointer: the hover tint would leave the light-on-dark digits unreadable. */
  &::part(selected):hover { background-color: var(--_accent); }
  &::part(range-inner) { background: var(--_range-background); color: var(--_color); border-radius: 0; }
  &::part(range-start) { border-start-end-radius: 0; border-end-end-radius: 0; }
  &::part(range-end) { border-start-start-radius: 0; border-end-start-radius: 0; }
  &::part(range-start range-end) { border-radius: var(--_border-radius); }
  &::part(outside) { opacity: 0.4; }
  /* Marks (see the header). Neither is by colour alone: a holiday is a dot under the
     number, a booked day has a hatched band along the bottom of the cell (the digits
     stay clean), and a pending booking's band is lighter. Only background-image is set
     here, and the hover, focus and disallowed rules below set background-color, so
     none of them wipes a mark. */
  &::part(holiday) {
    --_dot: radial-gradient(circle at 50% 85%, currentColor 0 0.14rem, transparent 0.16rem);
    background-image: var(--_dot);
  }
  &::part(booked) {
    --_band: repeating-linear-gradient(135deg, color-mix(in srgb, currentColor 55%, transparent) 0 2px, transparent 2px 5px);
    background-image: var(--_band);
    background-size: 100% 38%;
    background-position: 0 100%;
    background-repeat: no-repeat;
  }
  &::part(booked-pending) {
    --_band: repeating-linear-gradient(135deg, color-mix(in srgb, currentColor 30%, transparent) 0 2px, transparent 2px 7px);
    background-image: var(--_band);
  }
  &::part(holiday booked) {
    background-image: var(--_dot), var(--_band);
    background-size: auto, 100% 38%;
    background-position: 0 0, 0 100%;
  }
  &::part(disallowed) { opacity: 0.4; cursor: default; text-decoration: line-through; }
  &::part(disallowed):hover { background-color: transparent; }
  &::part(button):focus-visible {
    background-color: var(--_accent);
    color: var(--_accent-color);
    outline: 2px solid var(--_accent-color);
    outline-offset: -3px;
  }
}
`;

const MULTIPLE_CSS = /* css */ `
:host([multiple]) calendar-month::part(button) { touch-action: none; }
calendar-month::part(tentative) { outline: 2px dashed var(--_accent); outline-offset: -3px; background-color: var(--_range-background); color: var(--_color); }
.range-controls { max-inline-size: 33rem; }
.range-list { list-style: none; margin: .75rem 0; padding: 0; display: grid; gap: .5rem; }
.range-list li { display: flex; align-items: center; justify-content: space-between; gap: .75rem; }
.range-controls button { font: inherit; color: var(--_color); background: var(--_background); border: 1px solid var(--_border-color); border-radius: var(--_border-radius); padding: .375rem .625rem; cursor: pointer; }
.range-controls button:focus-visible { outline: var(--_focus-ring); outline-offset: 2px; }
/* Status updates must not resize the anchored popup and move a pressed day.
   Keep wrapped messages readable without letting their length affect layout. */
.range-status { font-size: .875em; margin-block: .5rem; max-inline-size: 30rem; block-size: 2lh; overflow: auto; }
`;

const SHEET = new CSSStyleSheet();
SHEET.replaceSync(CSS + MULTIPLE_CSS);

const CHEVRON =
  '<svg part="expand-icon" class="expand-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 6l4 4 4-4"/></svg>';
const CROSS =
  '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8"/></svg>';
const PREV =
  '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10 3L5 8l5 5"/></svg>';
const NEXT =
  '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 3l5 5-5 5"/></svg>';

const TEMPLATE = document.createElement('template');
TEMPLATE.innerHTML = `
<div part="form-control" class="form-control">
  <label part="form-control-label" id="label" for="field" hidden><slot name="label"><span id="label-text"></span></slot></label>
  <div part="base" class="base" id="base">
    <button part="input" class="field" id="field" type="button" aria-haspopup="dialog" aria-expanded="false" aria-describedby="hint">
      <span part="value" class="value" id="value"></span>
      ${CHEVRON}
    </button>
    <button part="clear-button" class="clear" id="clear" type="button" aria-label="Clear" hidden>${CROSS}</button>
  </div>
  <div part="hint" class="hint" id="hint" hidden><slot name="hint"><span id="hint-text"></span></slot></div>
  <div part="popup" class="popup" id="popup" popover="auto" role="dialog"></div>
  <div class="probe" id="probe" aria-hidden="true"></div>
  <div class="fallback" hidden><slot id="fallback"></slot></div>
</div>`;

const ISO = /^(\d{4})-(\d{2})-(\d{2})$/;

function isoValid(s) {
  const m = ISO.exec(s || '');
  if (!m) return false;
  const d = new Date(Date.UTC(+m[1], +m[2] - 1, +m[3]));
  return d.getUTCFullYear() === +m[1] && d.getUTCMonth() === +m[2] - 1 && d.getUTCDate() === +m[3];
}

/** Returns "start/end" (sorted) for a valid range, otherwise "" (blank). */
function normalize(v) {
  if (typeof v !== 'string') return '';
  const parts = v.trim().split('/');
  if (parts.length !== 2 || !isoValid(parts[0]) || !isoValid(parts[1])) return '';
  return parts[0] <= parts[1] ? `${parts[0]}/${parts[1]}` : `${parts[1]}/${parts[0]}`;
}

const utcDate = (iso) => {
  const [y, m, d] = iso.split('-').map(Number);
  return new Date(Date.UTC(y, m - 1, d));
};
const localDate = (iso) => {
  const [y, m, d] = iso.split('-').map(Number);
  return new Date(y, m - 1, d);
};
const toIso = (d) =>
  `${String(d.getFullYear()).padStart(4, '0')}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

// Cally hands day parts a Date at UTC midnight.
const utcIso = (d) => d.toISOString().slice(0, 10);

const STRING_ATTRS = ['label', 'hint', 'placeholder', 'min', 'max', 'locale', 'name', 'start-name', 'end-name', 'disabled-days-of-week', 'marks', 'max-ranges'];
const BOOL_ATTRS = ['required', 'disabled', 'readonly', 'with-clear', 'multiple'];
const camel = (s) => s.replace(/-(\w)/g, (_, c) => c.toUpperCase());

const UPGRADE_PROPS = [
  'value', 'valueAsRange', 'valueAsRanges', 'isDateDisabled', 'open', 'months', 'firstDayOfWeek', 'disabledDaysOfWeek',
  ...[...STRING_ATTRS, ...BOOL_ATTRS].map(camel),
];
const CALENDAR_ATTRS = ['min', 'max', 'locale', 'months', 'first-day-of-week', 'disabled-days-of-week', 'marks', 'multiple'];
const FALLBACK_CONTROLS = 'input, select, textarea, button[name]';

/** Accepts a string, or `{from, to}` with Dates or ISO strings. */
function valueFrom(v) {
  if (v && typeof v === 'object' && 'from' in v && 'to' in v) {
    const iso = (x) => (x instanceof Date ? toIso(x) : String(x));
    return normalize(`${iso(v.from)}/${iso(v.to)}`);
  }
  return normalize(String(v ?? ''));
}

// Range arithmetic uses UTC dates so DST never changes adjacency or splitting.
const shiftDay = (iso, amount) => utcIso(new Date(utcDate(iso).getTime() + amount * 864e5));
const serializeRanges = (ranges) => ranges.map(({ start, end }) => `${start}/${end}`).join(' ');
function mergedRanges(ranges) {
  const out = [];
  for (const r of ranges.map((r) => ({ ...r })).sort((a, b) => a.start.localeCompare(b.start))) {
    const last = out.at(-1);
    if (last && utcDate(r.start) - utcDate(last.end) <= 864e5) {
      if (r.end > last.end) last.end = r.end;
    } else out.push(r);
  }
  return out;
}
function multipleValue(v) {
  const values = Array.isArray(v) ? v.map((r) => r && 'start' in r ? `${r.start}/${r.end}` : valueFrom(r)) : String(v ?? '').trim().split(/\s+/);
  if (!values.length || values.some((r) => !normalize(r))) return '';
  return serializeRanges(mergedRanges(values.map((r) => {
    const [start, end] = normalize(r).split('/');
    return { start, end };
  })));
}
const eachDate = (ranges) => {
  const days = [];
  for (const { start, end } of ranges) {
    const last = utcDate(end).getTime();
    for (let t = utcDate(start).getTime(); t <= last; t += 864e5) days.push(utcIso(new Date(t)));
  }
  return days.join(' ');
};

export class DateRangeInput extends HTMLElement {
  static formAssociated = true;
  static observedAttributes = [...STRING_ATTRS, ...BOOL_ATTRS, 'value', 'months', 'first-day-of-week', 'open'];

  #internals;
  #root;
  #els = {};
  #calendar = null;
  #value = '';
  #defaultValue = '';
  #dirty = false;
  #touched = false;
  #custom = '';
  #formDisabled = false;
  #hiding = false;
  #hadFocus = false;
  #pointerWasOpen = false;
  #reflecting = false;
  #pendingOpen = false;
  #isDateDisabled = null;
  #pendingStart = '';
  #previewEnd = '';
  #drag = null;
  #gestureEvents = null;
  #swallowSelect = false;
  #swallowTimer;
  #paintTimer;
  #paintSettings = false;
  #multipleControls = null;
  #status = '';
  #observer = new MutationObserver(() => this.#adoptFallback());

  constructor() {
    super();
    this.#internals = this.attachInternals();
    this.#root = this.attachShadow({ mode: 'open', delegatesFocus: true });
    this.#root.adoptedStyleSheets = [SHEET];
    this.#root.append(TEMPLATE.content.cloneNode(true));
    for (const el of this.#root.querySelectorAll('[id]')) this.#els[camel(el.id)] = el;

    const { field, clear, popup } = this.#els;
    field.addEventListener('pointerdown', () => { this.#pointerWasOpen = this.open; });
    field.addEventListener('focus', () => this.#updateLabelling());
    field.addEventListener('click', () => {
      // A mouse press on the field already light-dismissed an open popup; don't reopen it.
      const was = this.#pointerWasOpen;
      this.#pointerWasOpen = false;
      if (was) return;
      this.open ? this.hide() : this.show();
    });
    field.addEventListener('keydown', (e) => {
      if (e.altKey && e.key === 'ArrowDown') { e.preventDefault(); this.show(); }
    });
    clear.addEventListener('click', () => { this.clear(); this.#els.field.focus(); });
    popup.addEventListener('beforetoggle', (e) => this.#onBeforeToggle(e));
    popup.addEventListener('toggle', (e) => this.#onToggle(e));
    popup.addEventListener('keydown', (e) => {
      this.#swallowSelect = false;
      if (this.multiple && e.key === 'Escape' && (this.#pendingStart || this.#drag)) {
        e.preventDefault();
        e.stopPropagation();
        this.#cancelMultiple('Selection cancelled.');
      } else if (this.multiple && e.key === 'Escape') {
        e.preventDefault();
        this.hide();
      }
    });
    popup.addEventListener('pointerdown', (e) => this.#pointerDown(e));
    popup.addEventListener('pointermove', (e) => this.#pointerMove(e));
    popup.addEventListener('pointerup', (e) => this.#pointerUp(e));
    popup.addEventListener('pointercancel', () => this.#cancelMultiple('Selection cancelled.'));
    this.#els.label.querySelector('slot').addEventListener('slotchange', () => this.#render());
    this.#els.hint.querySelector('slot').addEventListener('slotchange', () => this.#render());
    this.addEventListener('invalid', () => { this.#touched = true; this.#render(); });
  }

  connectedCallback() {
    // Properties set before this element was upgraded live on the instance and shadow the accessors.
    for (const p of UPGRADE_PROPS) {
      if (Object.hasOwn(this, p)) {
        const v = this[p];
        delete this[p];
        this[p] = v;
      }
    }
    this.#observer.observe(this, { childList: true, subtree: true });
    this.#adoptFallback();
    this.#render();
    if (this.hasAttribute('open')) this.show();
  }

  disconnectedCallback() {
    this.#observer.disconnect();
    clearTimeout(this.#paintTimer);
    this.#cancelMultiple('');
    // Removal hides the popover silently (no toggle event), so `open` is already false here.
    this.#reflect(false);
    this.#render();
  }

  attributeChangedCallback(name, _old, val) {
    if (name === 'open') {
      if (this.#reflecting) return;
      if (val === null) this.#pendingOpen = false;
      val !== null ? this.show() : this.hide();
      return;
    }
    if (name === 'value') {
      this.#defaultValue = this.#normalizeValue(val ?? '');
      if (!this.#dirty) this.#setValue(this.#defaultValue);
      return;
    }
    if (name === 'multiple') {
      this.#defaultValue = this.#normalizeValue(this.getAttribute('value') ?? '');
      this.#value = this.#normalizeValue(this.#dirty ? this.#value : this.#defaultValue);
      this.#cancelMultiple('');
    }
    if ((name === 'disabled' || name === 'readonly') && val !== null && this.open) this.hide();
    this.#render();
    if (CALENDAR_ATTRS.includes(name)) this.#refreshCalendar();
  }

  // ---- value ---------------------------------------------------------------

  get value() { return this.#value; }
  /** A `start/end` string, or `{from, to}` with Dates or ISO strings. */
  set value(v) {
    this.#dirty = true;
    this.#setValue(this.multiple ? multipleValue(v && !Array.isArray(v) && typeof v === 'object' ? [v] : v) : valueFrom(v));
  }
  get defaultValue() { return this.#defaultValue; }
  set defaultValue(v) { this.setAttribute('value', String(v ?? '')); }

  get ranges() {
    return this.#value ? this.#value.split(' ').map((r) => {
      const [start, end] = r.split('/');
      return { start, end };
    }) : [];
  }
  get start() { return this.ranges[0]?.start ?? ''; }
  get end() { return this.ranges.at(-1)?.end ?? ''; }
  get valueAsRanges() { return this.ranges.map(({ start, end }) => ({ from: localDate(start), to: localDate(end) })); }
  set valueAsRanges(v) { this.value = v; }
  #normalizeValue(v) { return this.multiple ? multipleValue(v) : normalize(v); }

  /** `{from: Date, to: Date}` at local midnight, or null when blank. */
  get valueAsRange() {
    return this.#value ? { from: localDate(this.start), to: localDate(this.end) } : null;
  }
  set valueAsRange(r) { this.value = r ?? ''; }

  /** Same as the clear button: blanks the value and fires `input` and `change`. */
  clear() {
    if (this.#isDisabled || this.readonly || !this.#value) return;
    this.#dirty = true;
    this.#touched = true;
    this.#setValue('');
    this.#emit('input');
    this.#emit('change');
  }

  #setValue(v) {
    this.#gestureEvents?.abort();
    this.#gestureEvents = null;
    this.#pendingStart = this.#previewEnd = '';
    this.#drag = null;
    this.#setStatus('');
    this.#value = v;
    this.#render();
    this.#refreshCalendar();
  }

  // ---- reflected properties ------------------------------------------------

  get months() { return this.getAttribute('months') === '2' ? 2 : 1; }
  set months(v) { this.setAttribute('months', String(v)); }
  /** Defaults to 1 (Monday), unlike Cally's own default of Sunday. */
  get firstDayOfWeek() {
    const n = parseInt(this.getAttribute('first-day-of-week') ?? '', 10);
    return n >= 0 && n <= 6 ? n : 1;
  }
  set firstDayOfWeek(v) { this.setAttribute('first-day-of-week', String(v)); }
  get disabledDaysOfWeek() { return this.getAttribute('disabled-days-of-week') ?? ''; }
  set disabledDaysOfWeek(v) { this.setAttribute('disabled-days-of-week', String(v)); }

  /** JS-only: return true for dates that cannot be picked. Receives a local-midnight Date. */
  get isDateDisabled() { return this.#isDateDisabled; }
  set isDateDisabled(fn) {
    this.#isDateDisabled = typeof fn === 'function' ? fn : null;
    this.#refreshCalendar();
  }

  // ---- form association ----------------------------------------------------

  get form() { return this.#internals.form; }
  get labels() { return this.#internals.labels; }
  get validity() { return this.#internals.validity; }
  get validationMessage() { return this.#internals.validationMessage; }
  get willValidate() { return this.#internals.willValidate; }
  checkValidity() { return this.#internals.checkValidity(); }
  reportValidity() { return this.#internals.reportValidity(); }
  setCustomValidity(msg) {
    this.#custom = String(msg ?? '');
    this.#render();
  }
  get type() { return 'date-range-input'; }

  formResetCallback() {
    this.#dirty = false;
    this.#touched = false;
    this.#setValue(this.#defaultValue);
  }
  formStateRestoreCallback(state) {
    this.#dirty = true;
    this.#setValue(this.#normalizeValue(String(state ?? '')));
  }
  formDisabledCallback(disabled) {
    this.#formDisabled = disabled;
    if (disabled && this.open) this.hide();
    this.#render();
  }

  get #isDisabled() { return this.disabled || this.#formDisabled; }

  #syncFormValue() {
    const { start, end, startName, endName } = this;
    let fd = null;
    if (this.multiple && this.#value && (startName || endName || this.name)) {
      fd = new FormData();
      for (const r of this.ranges) {
        if (startName || endName) {
          if (startName) fd.append(startName, r.start);
          if (endName) fd.append(endName, r.end);
        } else fd.append(this.name, `${r.start}/${r.end}`);
      }
    } else if (!this.#value) {
      fd = null;
    } else if (startName || endName) {
      fd = new FormData();
      if (startName) fd.append(startName, start);
      if (endName) fd.append(endName, end);
    } else if (this.name) {
      fd = `${start}/${end}`;
    }
    this.#internals.setFormValue(fd, this.#value);
  }

  #updateValidity() {
    const flags = {};
    let msg = '';
    const v = this.#value;
    if (!this.#isDisabled && !this.readonly) {
      if (this.required && !v) {
        flags.valueMissing = true;
        msg = 'Please select a date range.';
      }
      const min = isoValid(this.min) ? this.min : '';
      const max = isoValid(this.max) ? this.max : '';
      if (v && min && this.start < min) {
        flags.rangeUnderflow = true;
        msg = `Dates must be on or after ${this.#format(min)}.`;
      }
      if (v && max && this.end > max) {
        flags.rangeOverflow = true;
        msg = `Dates must be on or before ${this.#format(max)}.`;
      }
    }
    if (this.#custom) {
      flags.customError = true;
      msg = this.#custom;
    }
    if (Object.keys(flags).length) this.#internals.setValidity(flags, msg, this.#els.field);
    else this.#internals.setValidity({});
  }

  #updateStates() {
    const s = this.#internals.states;
    const set = (name, on) => (on ? s.add(name) : s.delete(name));
    set('range', true);
    set('blank', !this.#value);
    set('open', this.open);
    set('disabled', this.#isDisabled);
    set('required', this.required);
    set('user-invalid', this.#touched && !this.#internals.validity.valid);
  }

  // ---- rendering -----------------------------------------------------------

  /** First usable locale of: attribute, nearest [lang], <html lang>, the browser's languages. */
  get #locale() {
    const candidates = [this.locale, this.closest('[lang]')?.lang, document.documentElement.lang, ...navigator.languages];
    for (const l of candidates) {
      if (!l) continue;
      try {
        if (Intl.DateTimeFormat.supportedLocalesOf(l).length) return l;
      } catch { /* malformed tag, try the next */ }
    }
    return undefined;
  }

  #formatter() {
    return new Intl.DateTimeFormat(this.#locale, {
      weekday: 'short', day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC',
    });
  }

  #format(iso) { return this.#formatter().format(utcDate(iso)); }

  #formatRange(start = this.start, end = this.end) {
    return this.#formatter().formatRange(utcDate(start), utcDate(end));
  }

  #updateLabelling() {
    const { field, value } = this.#els;
    if (this.hasAttribute('label') || this.querySelector(':scope > [slot="label"]')) {
      field.setAttribute('aria-labelledby', 'label value');
    } else {
      // An external <label for="..."> pointing at the host.
      field.removeAttribute('aria-labelledby');
      field.ariaLabelledByElements = [...this.#internals.labels, value];
    }
  }

  #render() {
    const { label, labelText, hint, hintText, field, value, clear, popup } = this.#els;
    const blank = !this.#value;
    const hasLabel = this.hasAttribute('label') || !!this.querySelector(':scope > [slot="label"]');
    const hasHint = this.hasAttribute('hint') || !!this.querySelector(':scope > [slot="hint"]');
    labelText.textContent = this.label;
    hintText.textContent = this.hint;
    label.hidden = !hasLabel;
    hint.hidden = !hasHint;
    value.textContent = blank ? this.placeholder || ' ' : this.ranges.map(({ start, end }) => this.#formatRange(start, end)).join(', ');
    this.#updateLabelling();
    if (hasHint) field.setAttribute('aria-describedby', 'hint');
    else field.removeAttribute('aria-describedby');
    field.disabled = this.#isDisabled;
    field.setAttribute('aria-expanded', String(this.open));
    field.toggleAttribute('aria-required', this.required);
    clear.hidden = !(this.withClear && !blank && !this.#isDisabled && !this.readonly);
    popup.setAttribute('aria-label', this.label ? `${this.label} calendar` : 'Date range calendar');
    this.#syncFormValue();
    this.#updateValidity();
    this.#updateStates();
    field.setAttribute('aria-invalid', String(this.#touched && !this.#internals.validity.valid));
  }

  // ---- calendar ------------------------------------------------------------

  /**
   * Cally pages relative to its history after first render, and 0.9.2 spins forever if a
   * connected element has `value` / `focusedDate` set (or is moved) around connect/disconnect in
   * one task. So every open builds a FRESH calendar with all state set before it is connected.
   */
  #buildCalendar(focusedDate) {
    const cal = document.createElement(this.multiple ? 'calendar-multi' : 'calendar-range');
    cal.setAttribute('part', 'calendar');
    cal.setAttribute('exportparts',
      'container:calendar-container,header:calendar-header,heading:calendar-heading,previous:previous-button,next:next-button');
    cal.innerHTML =
      `<span slot="previous">${PREV}<span class="vh">Previous month</span></span>` +
      `<span slot="next">${NEXT}<span class="vh">Next month</span></span>` +
      '<div class="months" part="calendar-months"></div>';
    const months = this.#visibleMonths;
    const wrap = cal.querySelector('.months');
    for (let i = 0; i < months; i++) {
      const m = document.createElement('calendar-month');
      m.setAttribute('exportparts',
        'heading:month-heading,table:month-table,th:weekday,td:day-cell,button:day-button,selected:day-selected,' +
        'today:day-today,disallowed:day-disallowed,outside:day-outside,range-start:day-range-start,' +
        'range-end:day-range-end,range-inner:day-range-inner,holiday:day-holiday,booked:day-booked,' +
        'booked-approved:day-booked-approved,booked-pending:day-booked-pending,tentative:day-tentative');
      m.setAttribute('part', 'calendar-month');
      m.offset = i;
      if (this.multiple) {
        m.addEventListener('selectday', (e) => {
          e.stopPropagation();
          if (this.#swallowSelect) { this.#swallowSelect = false; return; }
          this.#activateDay(String(e.detail));
        });
        for (const type of ['hoverday', 'focusday']) m.addEventListener(type, (e) => {
          if (!this.#pendingStart || this.#drag) return;
          const iso = String(e.detail);
          if (this.#canPick(iso)) { this.#previewEnd = iso; this.#schedulePaint(); }
        });
      }
      wrap.append(m);
    }
    const min = isoValid(this.min) ? this.min : '';
    const max = isoValid(this.max) ? this.max : '';
    cal.months = months;
    cal.min = min;
    cal.max = max;
    cal.locale = this.#locale;
    cal.firstDayOfWeek = this.firstDayOfWeek;
    cal.isDateDisallowed = this.#isDisallowed;
    const dayParts = this.#dayParts();
    if (this.multiple) { cal.getDayParts = this.#multipleParts(dayParts); cal.value = eachDate(this.ranges); }
    else {
      if (dayParts) cal.getDayParts = dayParts;
      if (this.#value) cal.value = this.#value;
    }
    const today = toIso(new Date());
    cal.focusedDate = focusedDate || this.start || (min && today < min ? min : max && today > max ? max : undefined);
    cal.addEventListener('change', (e) => {
      e.stopPropagation();
      if (!this.multiple) this.#onCalendarChange();
    });
    cal.addEventListener('rangestart', (e) => {
      e.stopPropagation();
      const d = e.detail;
      const start = d instanceof Date
        ? `${String(d.getUTCFullYear()).padStart(4, '0')}-${String(d.getUTCMonth() + 1).padStart(2, '0')}-${String(d.getUTCDate()).padStart(2, '0')}`
        : String(d);
      this.dispatchEvent(new CustomEvent('dri-range-start', { bubbles: true, composed: true, detail: { start } }));
    });
    cal.addEventListener('rangeend', (e) => e.stopPropagation());
    return cal;
  }

  /** `months` is a maximum: two only when the viewport is at least --dri-two-months-min-width wide. */
  get #visibleMonths() {
    if (this.months < 2) return 1;
    const needed = this.#els.probe.getBoundingClientRect().width;
    return document.documentElement.clientWidth >= needed ? 2 : 1;
  }

  #installCalendar(focusedDate) {
    const cal = this.#buildCalendar(focusedDate);
    const old = this.#calendar;
    this.#calendar = cal;
    if (old) old.replaceWith(cal);
    else this.#els.popup.prepend(cal);
    this.#renderRangeControls();
  }

  /** While open, replaces the calendar so it reflects the current value and settings. */
  #refreshCalendar() {
    if (!this.open) return;
    if (this.multiple && this.#calendar?.localName === 'calendar-multi') {
      this.#schedulePaint(true);
      this.#renderRangeControls();
      return;
    }
    this.#installCalendar();
    this.#focusCalendar();
  }

  // Connected calendar-multi updates are safe once settled (see demo spike). Coalesce
  // them into a later task: a caller may set value and remove/reparent us in one task.
  #schedulePaint(settings = false) {
    this.#paintSettings ||= settings;
    clearTimeout(this.#paintTimer);
    const cal = this.#calendar;
    this.#paintTimer = setTimeout(() => {
      if (!this.multiple || !this.open || !this.isConnected || this.#calendar !== cal || !cal?.isConnected) return;
      const updateSettings = this.#paintSettings;
      this.#paintSettings = false;
      if (updateSettings && cal.months !== this.#visibleMonths) {
        this.#installCalendar(cal.focusedDate);
        this.#focusCalendar();
        return;
      }
      if (updateSettings) {
        cal.min = isoValid(this.min) ? this.min : '';
        cal.max = isoValid(this.max) ? this.max : '';
        cal.locale = this.#locale;
        cal.firstDayOfWeek = this.firstDayOfWeek;
        cal.isDateDisallowed = this.#isDisallowed;
      }
      cal.value = eachDate(this.ranges);
      cal.getDayParts = this.#multipleParts(this.#dayParts());
    });
  }

  #multipleParts(marks) {
    const ranges = this.ranges;
    const start = this.#drag?.origin || this.#pendingStart;
    const end = this.#drag?.end || this.#previewEnd || start;
    const low = start < end ? start : end;
    const high = start < end ? end : start;
    return (date) => {
      const iso = utcIso(date);
      const out = [`d-${iso}`, marks?.(date) || ''];
      for (const r of ranges) {
        if (iso === r.start) out.push('range-start');
        if (iso === r.end) out.push('range-end');
        if (r.start < iso && iso < r.end) out.push('range-inner');
      }
      if (start && low <= iso && iso <= high) out.push('tentative');
      return out.join(' ');
    };
  }

  #canPick(iso) {
    return this.multiple && !this.#isDisabled && !this.readonly && isoValid(iso)
      && !(isoValid(this.min) && iso < this.min) && !(isoValid(this.max) && iso > this.max)
      && !this.#isDisallowed(utcDate(iso));
  }

  #selected(iso) { return this.ranges.some((r) => r.start <= iso && iso <= r.end); }

  #setStatus(message) {
    this.#status = message;
    const status = this.#multipleControls?.querySelector('[part="status"]');
    if (status) status.textContent = message;
  }

  #renderRangeControls() {
    if (!this.multiple) {
      this.#multipleControls?.remove();
      this.#multipleControls = null;
      return;
    }
    if (!this.#multipleControls) {
      const controls = document.createElement('div');
      controls.className = 'range-controls';
      const list = document.createElement('ul');
      list.className = 'range-list';
      list.setAttribute('part', 'range-list');
      const status = document.createElement('p');
      status.className = 'range-status';
      status.setAttribute('part', 'status');
      status.setAttribute('aria-live', 'polite');
      status.setAttribute('aria-atomic', 'true');
      const done = document.createElement('button');
      done.type = 'button';
      done.setAttribute('part', 'done-button');
      done.textContent = 'Done';
      done.addEventListener('click', () => this.hide());
      controls.append(list, status, done);
      this.#multipleControls = controls;
      this.#els.popup.append(controls);
    }
    const list = this.#multipleControls.querySelector('ul');
    const focusedRemove = [...list.querySelectorAll('button')].indexOf(this.#root.activeElement);
    const items = this.ranges.map((r, index) => {
      const item = document.createElement('li');
      const text = document.createElement('span');
      text.textContent = this.#formatRange(r.start, r.end);
      const remove = document.createElement('button');
      remove.type = 'button';
      remove.textContent = 'Remove';
      remove.setAttribute('part', 'range-remove');
      remove.setAttribute('aria-label', `Remove ${text.textContent}`);
      remove.addEventListener('click', () => {
        this.#commitRanges(this.ranges.filter((x) => x.start !== r.start), 'Removed range.');
        const buttons = list.querySelectorAll('button');
        (buttons[Math.min(index, buttons.length - 1)] || this.#multipleControls.querySelector('[part="done-button"]') || this.#els.field).focus();
      });
      item.append(text, remove);
      return item;
    });
    list.replaceChildren(...items);
    this.#setStatus(this.#status);
    if (focusedRemove >= 0) (items[Math.min(focusedRemove, items.length - 1)]?.querySelector('button') || this.#multipleControls.querySelector('[part="done-button"]') || this.#els.field).focus();
  }

  #commitRanges(ranges, message) {
    const merged = mergedRanges(ranges);
    const limit = Number(this.maxRanges);
    if (this.maxRanges && Number.isInteger(limit) && limit >= 0 && merged.length > limit && merged.length > this.ranges.length) {
      this.#pendingStart = this.#previewEnd = '';
      this.#drag = null;
      this.#setStatus(`Choose at most ${limit} ranges.`);
      this.#schedulePaint();
      return;
    }
    const value = serializeRanges(merged);
    const changed = value !== this.#value;
    this.#dirty = true;
    this.#touched = true;
    this.#setValue(value);
    this.#setStatus(`${message} ${merged.length} ${merged.length === 1 ? 'range' : 'ranges'}.`);
    if (changed) this.#emit('change');
  }

  #applySpan(a, b, erase) {
    if (!this.#canPick(a) || !this.#canPick(b)) return;
    const start = a < b ? a : b;
    const end = a < b ? b : a;
    let ranges;
    if (!erase) ranges = [...this.ranges, { start, end }];
    else {
      ranges = [];
      for (const r of this.ranges) {
        if (r.end < start || r.start > end) ranges.push(r);
        else {
          // Only the newly exposed edges need to be pickable. Trim them inward;
          // preserve old endpoints and unrelated ranges, even outside min/max.
          if (r.start < start) {
            let pieceEnd = shiftDay(start, -1);
            while (pieceEnd >= r.start && !this.#canPick(pieceEnd)) pieceEnd = shiftDay(pieceEnd, -1);
            if (pieceEnd >= r.start) ranges.push({ start: r.start, end: pieceEnd });
          }
          if (r.end > end) {
            let pieceStart = shiftDay(end, 1);
            while (pieceStart <= r.end && !this.#canPick(pieceStart)) pieceStart = shiftDay(pieceStart, 1);
            if (pieceStart <= r.end) ranges.push({ start: pieceStart, end: r.end });
          }
        }
      }
    }
    this.#commitRanges(ranges, `${erase ? 'Removed' : 'Added'} ${this.#formatRange(start, end)}.`);
  }

  #activateDay(iso) {
    if (!this.#canPick(iso)) { this.#setStatus('That day is unavailable.'); return; }
    if (this.#pendingStart) { this.#applySpan(this.#pendingStart, iso, false); return; }
    if (this.#selected(iso)) { this.#applySpan(iso, iso, true); return; }
    this.#pendingStart = this.#previewEnd = iso;
    this.#setStatus(`Start chosen: ${this.#format(iso)}. Choose the last day.`);
    this.#schedulePaint();
    this.dispatchEvent(new CustomEvent('dri-range-start', { bubbles: true, composed: true, detail: { start: iso } }));
  }

  #dayAtPoint(x, y) {
    for (const month of this.#calendar?.querySelectorAll('calendar-month') || []) {
      const button = month.shadowRoot?.elementFromPoint(x, y)?.closest('button');
      if (!button || button.part.contains('outside')) continue;
      const token = [...button.part].find((part) => /^d-\d{4}-\d{2}-\d{2}$/.test(part));
      if (token) return token.slice(2);
    }
    return '';
  }

  #pointerDown(e) {
    if (!this.multiple || !this.open || !e.isPrimary || e.button !== 0) return;
    // Clear any suppression left by a browser that did not generate a click.
    this.#swallowSelect = false;
    clearTimeout(this.#swallowTimer);
    const origin = this.#dayAtPoint(e.clientX, e.clientY);
    if (!this.#canPick(origin)) return;
    this.#setStatus('');
    this.#gestureEvents?.abort();
    this.#gestureEvents = new AbortController();
    const { signal } = this.#gestureEvents;
    // Before capture starts a release may occur outside the popup entirely.
    // Listen only for this gesture and tear the listeners down on every exit.
    window.addEventListener('pointerup', (event) => this.#pointerUp(event), { signal });
    window.addEventListener('pointercancel', (event) => {
      if (this.#drag?.id === event.pointerId) this.#cancelMultiple('Selection cancelled.');
    }, { signal });
    this.#drag = { id: e.pointerId, origin, end: origin, erase: this.#selected(origin), moved: false };
  }

  #pointerMove(e) {
    const drag = this.#drag;
    if (!drag || e.pointerId !== drag.id) return;
    const day = this.#dayAtPoint(e.clientX, e.clientY);
    if (this.#canPick(day)) {
      drag.end = day;
      drag.moved ||= day !== drag.origin;
      if (drag.moved) {
        try { this.#els.popup.setPointerCapture(e.pointerId); } catch { /* synthetic pointer */ }
        e.preventDefault();
        this.#schedulePaint();
      }
    }
  }

  #pointerUp(e) {
    const drag = this.#drag;
    if (!drag || e.pointerId !== drag.id) return;
    const here = this.#dayAtPoint(e.clientX, e.clientY);
    const end = this.#canPick(here) ? here : drag.end;
    this.#gestureEvents?.abort();
    this.#gestureEvents = null;
    this.#drag = null;
    try { this.#els.popup.releasePointerCapture(e.pointerId); } catch { /* synthetic pointer */ }
    if (drag.moved) {
      this.#swallowSelect = true;
      clearTimeout(this.#swallowTimer);
      // A compatibility click follows pointerup. New pointer presses and keyboard
      // events clear this flag, and expiry handles browsers that omit that click.
      this.#swallowTimer = setTimeout(() => { this.#swallowSelect = false; }, 500);
      this.#pendingStart = this.#previewEnd = '';
      if (end === drag.origin) this.#activateDay(end);
      else if (this.#canPick(end)) this.#applySpan(drag.origin, end, drag.erase);
      else this.#setStatus('That day is unavailable.');
    }
    this.#schedulePaint();
  }

  #cancelMultiple(message) {
    this.#gestureEvents?.abort();
    this.#gestureEvents = null;
    if (this.#drag) {
      try { this.#els.popup.releasePointerCapture(this.#drag.id); } catch { /* no active pointer */ }
    }
    this.#pendingStart = this.#previewEnd = '';
    this.#drag = null;
    this.#setStatus(message);
    if (this.multiple) this.#schedulePaint();
  }


  /** Cally's getDayParts for the `marks` data, or null when there is none or it is not valid JSON. */
  #dayParts() {
    const id = this.marks.replace(/^#/, '');
    const el = id && this.getRootNode().getElementById?.(id);
    let data;
    try { data = el && JSON.parse(el.textContent); } catch { return null; }
    if (!data || typeof data !== 'object') return null;
    const holidays = data.holidays && typeof data.holidays === 'object' ? data.holidays : {};
    const booked = new Map();
    for (const r of Array.isArray(data.booked) ? data.booked : []) {
      if (!r || !isoValid(r.start) || !isoValid(r.end)) continue;
      // Status becomes part of a token list, so keep it to plain word characters.
      const status = String(r.status ?? '').replace(/[^a-z-]/gi, '');
      const last = utcDate(r.end).getTime();
      for (let t = utcDate(r.start).getTime(), n = 0; t <= last && n < 800; t += 864e5, n++) {
        const k = utcIso(new Date(t));
        // Approved wins over pending when ranges overlap on a day.
        if (booked.get(k) !== 'approved') booked.set(k, status);
      }
    }
    return (d) => {
      const k = utcIso(d);
      const out = [];
      if (Object.hasOwn(holidays, k)) out.push('holiday');
      if (booked.has(k)) out.push('booked', booked.get(k) && `booked-${booked.get(k)}`);
      return out.filter(Boolean).join(' ');
    };
  }

  #isDisallowed = (d) => {
    const days = this.disabledDaysOfWeek.split(',').map((s) => parseInt(s, 10)).filter((n) => n >= 0 && n <= 6);
    if (days.includes(d.getUTCDay())) return true;
    if (this.#isDateDisabled) {
      return !!this.#isDateDisabled(new Date(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()));
    }
    return false;
  };

  #focusCalendar() {
    const cal = this.#calendar;
    // Cally renders in a microtask, so by the next task its day buttons exist.
    setTimeout(() => {
      if (this.open && this.#calendar === cal) cal.focus();
    });
  }

  #onCalendarChange() {
    const v = normalize(this.#calendar.value);
    if (!v) return;
    const changed = v !== this.#value;
    this.#dirty = true;
    this.#touched = true;
    this.#close(true);
    this.#setValue(v);
    if (changed) this.#emit('change');
  }

  // ---- popover -------------------------------------------------------------

  get open() { return this.#els.popup.matches(':popover-open'); }
  set open(v) { this.toggleAttribute('open', !!v); }

  #reflect(on) {
    this.#reflecting = true;
    this.toggleAttribute('open', on);
    this.#reflecting = false;
  }

  show() {
    if (this.open || !this.isConnected) return;
    if (this.#isDisabled || this.readonly) {
      this.#reflect(false);
      return;
    }
    if (!customElements.get(this.multiple ? 'calendar-multi' : 'calendar-range')) {
      // Cally not loaded (yet): open once it registers, if still requested.
      this.#reflect(false);
      if (!this.#pendingOpen) {
        this.#pendingOpen = true;
        customElements.whenDefined(this.multiple ? 'calendar-multi' : 'calendar-range').then(() => {
          if (this.#pendingOpen) {
            this.#pendingOpen = false;
            this.show();
          }
        });
      }
      return;
    }
    if (!this.#emit('dri-show', true, true)) {
      this.#reflect(false);
      return;
    }
    this.#installCalendar();
    this.#els.popup.showPopover();
    this.#reflect(true);
  }

  hide() {
    this.#pendingOpen = false;
    if (!this.open) return;
    if (!this.#emit('dri-hide', true, true)) {
      this.#reflect(true);
      return;
    }
    this.#hiding = true;
    try { this.#els.popup.hidePopover(); } finally { this.#hiding = false; }
    this.#reflect(false);
  }

  /** Closes without a cancelable dri-hide (used after a completed selection). */
  #close(returnFocus) {
    if (!this.open) return;
    this.#hadFocus = returnFocus;
    this.#hiding = true;
    this.#emit('dri-hide', false, true);
    try { this.#els.popup.hidePopover(); } finally { this.#hiding = false; }
    this.#reflect(false);
  }

  #onBeforeToggle(e) {
    if (e.newState === 'closed') {
      this.#cancelMultiple('');
      this.#hadFocus = this.#hadFocus || this.#els.popup.matches(':focus-within');
      // Escape and light dismiss can't be cancelled, so they report a non-cancelable dri-hide.
      if (!this.#hiding) {
        this.#emit('dri-hide', false, true);
        this.#reflect(false);
      }
    }
  }

  #onToggle(e) {
    const open = e.newState === 'open';
    this.#render();
    if (open) {
      this.#focusCalendar();
      this.#emit('dri-after-show', false, true);
      return;
    }
    if (this.#hadFocus) this.#els.field.focus();
    this.#hadFocus = false;
    this.#emit('dri-after-hide', false, true);
  }

  // ---- misc ----------------------------------------------------------------

  /** `input` and `change` are not composed, like the native events. */
  #emit(name, cancelable = false, composed = false) {
    return this.dispatchEvent(new Event(name, { bubbles: true, composed, cancelable }));
  }

  /**
   * Disables native controls placed inside the element for no-JS use (wherever they sit in the
   * subtree), and seeds the value from the date inputs.
   */
  #adoptFallback() {
    const controls = [...this.querySelectorAll(FALLBACK_CONTROLS)].filter((c) => !c.closest('[slot]'));
    for (const c of controls) c.disabled = true;
    if (this.hasAttribute('value') || this.#dirty) return;
    const dates = controls.filter((c) => c.type === 'date');
    if (dates.length < 2) return;
    const v = normalize(`${dates[0].value}/${dates[1].value}`);
    if (v === this.#defaultValue && v === this.#value) return;
    this.#defaultValue = v;
    this.#setValue(v);
  }
}

for (const attr of STRING_ATTRS) {
  const prop = camel(attr);
  if (prop === 'disabledDaysOfWeek') continue;
  Object.defineProperty(DateRangeInput.prototype, prop, {
    get() { return this.getAttribute(attr) ?? ''; },
    set(v) { this.setAttribute(attr, String(v ?? '')); },
    enumerable: true,
    configurable: true,
  });
}
for (const attr of BOOL_ATTRS) {
  Object.defineProperty(DateRangeInput.prototype, camel(attr), {
    get() { return this.hasAttribute(attr); },
    set(v) { this.toggleAttribute(attr, !!v); },
    enumerable: true,
    configurable: true,
  });
}

if (!customElements.get('date-range-input')) customElements.define('date-range-input', DateRangeInput);
