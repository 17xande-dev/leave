// The apply form's one piece of script: keep the date picker's earliest
// selectable day in step with the chosen leave type, because each type has its
// own backdating limit. The server renders a data-min (YYYY-MM-DD) on every
// option and enforces the same limit when it quotes and saves; this only
// greys out what it would refuse.
const type = document.getElementById('type');
const picker = document.querySelector('date-range-input');

if (type && picker) {
  const fallbacks = picker.querySelectorAll('input[type="date"]');
  // With no type chosen the picker allows the earliest day any type allows.
  const earliest = [...type.options].map((o) => o.dataset.min).filter(Boolean).sort()[0] || '';

  const sync = () => {
    const min = type.selectedOptions[0]?.dataset.min || earliest;
    // Attributes only: setting one fires no change event, so the preview does
    // not refresh twice. A value already before the new min is left alone; the
    // preview shows the server's message and the picker reports rangeUnderflow
    // on submit.
    for (const el of [picker, ...fallbacks]) {
      if (min) el.setAttribute('min', min);
      else el.removeAttribute('min');
    }
  };

  type.addEventListener('change', sync);
  // The browser restores the select (but not our attribute) on Back or a
  // reload, and a page restored from the back/forward cache never re-runs this
  // module, so sync on load and on pageshow too.
  sync();
  addEventListener('pageshow', sync);
}
