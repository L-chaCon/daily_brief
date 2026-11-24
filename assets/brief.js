(() => {
  // Checkboxes: state per day in localStorage, key brief:<date>:<item id>. A new day is a new list.
  const date = document.body.dataset.date;
  const key = (id) => `brief:${date}:${id}`;
  for (const box of document.querySelectorAll("input[data-check]")) {
    const card = box.closest(".card");
    const paint = () => card && card.classList.toggle("done", box.checked);
    box.checked = localStorage.getItem(key(box.dataset.check)) === "1";
    paint();
    box.addEventListener("change", () => {
      if (box.checked) localStorage.setItem(key(box.dataset.check), "1");
      else localStorage.removeItem(key(box.dataset.check));
      paint();
    });
  }

  // Copy buttons: put the prompt on the clipboard; paste it into whichever AI you like.
  for (const btn of document.querySelectorAll("button[data-copy]")) {
    const label = btn.textContent;
    btn.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(btn.dataset.copy);
        btn.textContent = "Copied ✓";
      } catch {
        btn.textContent = "Copy failed";
      }
      setTimeout(() => (btn.textContent = label), 2000);
    });
  }

  // Keyboard: j/k move between cards, x toggles the current card's checkbox.
  const cards = [...document.querySelectorAll(".card")];
  let i = -1;
  const focus = (n) => {
    if (!cards.length) return;
    i = Math.max(0, Math.min(cards.length - 1, n));
    cards[i].scrollIntoView({ block: "center", behavior: "smooth" });
    cards.forEach((c, j) => (c.style.outline = j === i ? "2px solid var(--accent)" : ""));
  };
  document.addEventListener("keydown", (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey || e.target.matches("input, textarea")) return;
    if (e.key === "j") focus(i + 1);
    else if (e.key === "k") focus(i - 1);
    else if (e.key === "x" && i >= 0) cards[i].querySelector("input[data-check]")?.click();
  });
})();
