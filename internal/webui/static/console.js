// Console behaviour. This loads in <head> without defer, so its listeners,
// all on the document, exist before the body parses and keep working after
// htmx swaps it. Every page also works without it.
(function () {
  // A connector logo that can't load (offline, say) hides, which shows the
  // letter under it. error doesn't bubble, so listen in the capture phase.
  document.addEventListener(
    "error",
    (e) => {
      if (e.target instanceof HTMLImageElement && e.target.matches("img.brand-logo")) {
        e.target.hidden = true;
      }
    },
    true,
  );

  // A multi-select dropdown: a <details class="multi"> of checkboxes, which
  // opens and posts with no script. This adds the filter, keeps the
  // summary's chips in step with the boxes, and closes it on Escape or when
  // focus or a click goes elsewhere.
  function closeOthers(target) {
    for (const d of document.querySelectorAll("details.multi[open]")) {
      if (!d.contains(target)) {
        d.open = false;
      }
    }
  }

  function filter(input) {
    const menu = input.closest(".multi-menu");
    const q = input.value.trim().toLowerCase();
    let shown = 0;

    for (const row of menu.querySelectorAll(".check")) {
      row.hidden = q !== "" && !row.textContent.toLowerCase().includes(q);
      shown += row.hidden ? 0 : 1;
    }

    menu.querySelector(".multi-none").hidden = shown > 0;
  }

  function renderChips(multi) {
    const value = multi.querySelector(".multi-value");

    for (const chip of value.querySelectorAll(".chip")) {
      chip.remove();
    }

    const placeholder = value.querySelector(".multi-placeholder");

    for (const box of multi.querySelectorAll("input[type=checkbox]:checked")) {
      const chip = document.createElement("span");
      chip.className = "badge secondary chip";
      const logo = box.parentElement.querySelector(".mini");

      if (logo) {
        chip.append(logo.cloneNode(true));
      }

      chip.append(box.value);
      value.insertBefore(chip, placeholder);
    }
  }

  document.addEventListener("click", (e) => closeOthers(e.target));
  document.addEventListener("focusin", (e) => closeOthers(e.target));

  document.addEventListener("input", (e) => {
    if (e.target.matches(".multi-filter")) {
      filter(e.target);
    }
  });

  document.addEventListener("change", (e) => {
    const multi = e.target.closest("details.multi");

    if (multi && e.target.type === "checkbox") {
      renderChips(multi);
    }
  });

  document.addEventListener("keydown", (e) => {
    const multi = e.target.closest && e.target.closest("details.multi[open]");

    if (!multi) {
      return;
    }

    if (e.key === "Escape") {
      e.preventDefault();
      multi.open = false;
      multi.querySelector("summary").focus();
    } else if (e.key === "Enter" && e.target.matches(".multi-filter")) {
      // Enter in the filter would submit the form.
      e.preventDefault();
    }
  });

  // toggle does not bubble, so listen in the capture phase.
  document.addEventListener(
    "toggle",
    (e) => {
      if (e.target.matches && e.target.matches("details.multi") && e.target.open) {
        e.target.querySelector(".multi-filter").focus();
      }
    },
    true,
  );

  // A polled region marked data-poll-pause (the approvals table) skips its
  // refresh while the pointer is over it or focus is in it, and for 5
  // seconds after any touch, click, scroll, or key anywhere on the page, so
  // the swap can't move or replace what the owner is reading or about to
  // press. A poll already in flight when one of those happens has its
  // answer dropped. An hx-trigger filter would say this in the markup, but
  // filters need eval, which the console's htmx config turns off. Only the
  // region's own poll is held: a form inside it is the request's element,
  // not the region.
  let lastUse = 0;

  for (const type of ["touchstart", "pointerdown", "scroll", "keydown"]) {
    // scroll doesn't bubble from an element, so listen in the capture
    // phase; passive, since nothing here cancels the event.
    document.addEventListener(
      type,
      () => {
        lastUse = Date.now();
      },
      { capture: true, passive: true },
    );
  }

  function pollPaused(el) {
    return (
      el &&
      el.matches &&
      el.matches("[data-poll-pause]") &&
      (el.matches(":hover, :focus-within") || Date.now() - lastUse < 5000)
    );
  }

  document.addEventListener("htmx:beforeRequest", (e) => {
    if (pollPaused(e.detail && e.detail.elt)) {
      e.preventDefault();
    }
  });

  // beforeSwap fires on the swap's target, which for the region's poll is
  // the region itself (hx-target="this").
  document.addEventListener("htmx:beforeSwap", (e) => {
    if (pollPaused(e.detail && e.detail.elt)) {
      e.detail.shouldSwap = false;
    }
  });

  // Copy buttons: shown (by CSS) only when the page can write to the
  // clipboard. The text they copy is selectable without any of this.
  if (navigator.clipboard && navigator.clipboard.writeText) {
    document.documentElement.classList.add("can-copy");
  }

  function select(el) {
    if (el.select) {
      el.select();
      return;
    }

    const range = document.createRange();
    range.selectNodeContents(el);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }

  document.addEventListener("click", async (e) => {
    const btn = e.target.closest && e.target.closest("[data-copy]");

    if (!btn) {
      return;
    }

    const src = document.querySelector(btn.dataset.copy);
    const label = btn.querySelector("span");

    if (!src || !label) {
      return;
    }

    const was = btn.dataset.label || label.textContent;
    btn.dataset.label = was;

    try {
      await navigator.clipboard.writeText(("value" in src ? src.value : src.textContent).trim());
      label.textContent = "Copied";
    } catch {
      // The write was refused (permissions, an insecure context): select the
      // text so the user can copy it by hand.
      select(src);
      label.textContent = "Press Ctrl+C";
    }

    setTimeout(() => {
      label.textContent = was;
    }, 2500);
  });
})();
