# Genesis — a guide for people who don't write code

You describe the thing you want. Genesis builds it. This explains what you get
and what to do with it, and assumes no technical background.

---

## 1. Describe what you want

Open Genesis and type what you need in plain English:

> Build an online shop with products, a basket and customer accounts

Then click **Build it**.

Be specific about the *things* in your business — products, orders, customers,
bookings, invoices. Genesis turns each one into a part of the system. "Build a
shop" works; "Build an online shop that sells handmade furniture, where
customers can leave reviews and I can track stock" works better.

## 2. Wait — a few seconds to a couple of minutes

You will see the steps as they happen:

| Step | What is happening |
|---|---|
| Product Analysis | Working out what to build from your description |
| Design & Architecture | Designing the screens and how information is stored |
| Task Planning | Breaking the work into pieces |
| Code Generation | Writing the actual code |
| Testing & Review | **Compiling and running it to check it works** |
| Self Healing | Fixing anything that failed |
| Packaging & Deployment | Adding setup files and documentation |

**Testing & Review is the slow part**, and this matters: Genesis does not just
write code and hope. It compiles the project, runs the tests, starts the
program and checks it answers. That is why it takes longer than a chatbot — and
why what you get actually runs.

## 3. Your project is ready

When it finishes you see a summary: how many files, how many documents, and
three buttons.

### Download project ← start here

Saves everything as a `.zip` on your computer. Unzip it and you have an ordinary
folder of code. Nothing is locked to Genesis; you can email it to a developer,
put it on GitHub, or keep it.

### View the code

Opens the built-in editor. Browse the files, read them, change them. Every
change is saved and can be undone.

### Open folder

Shows the project in your normal file manager (Finder, Explorer).

---

## What you actually received

A real project, in three parts:

```
your-project/
├── api/          The server — data, rules, security
├── web/          The website people see
├── migrations/   Sets up the database
├── docs/         Requirements, architecture, test plans
└── README.md     Instructions for a developer
```

Plus documents a team would normally write by hand: what the product should do,
how it is designed, how it is tested, what to watch in production.

---

## Running it yourself

You need three free programs installed first: **Go**, **Node.js** and
**PostgreSQL**. If those words mean nothing to you, this is the point to hand
the folder to a developer — `README.md` inside is written for them.

If you'd like to try:

**1. Set up the database** (once)
```
createdb app
psql -d app -f migrations/0001_init.up.sql
```

**2. Start the server**
```
cd api
go run ./cmd/server
```

**3. Start the website** (in a second window)
```
cd web
npm install
npm run dev
```

Then open **http://localhost:5173** in your browser.

The delivery screen has a **Copy** button next to each of these.

---

## Common questions

**Does this cost anything?**
No. No account, no subscription, no API key. Genesis runs entirely on your
computer and never contacts a paid service. You can disconnect from the
internet and it still works.

**Is the code really mine?**
Yes. It is ordinary code in ordinary languages. No licence check, no
phone-home, nothing that stops working if you stop using Genesis.

**Can I change it later?**
Yes — in the built-in editor, or in any code editor after downloading. It is a
normal project.

**Why does it take longer than ChatGPT?**
Because it compiles and runs what it writes. A chatbot hands you text that
looks like code. Genesis hands you a project that has been built and tested.

**Something says "not responding".**
The engine that does the work failed to start. Click **Restart engine** on that
screen. If it persists, **View log** shows why.

**I asked for something and got something slightly different.**
Genesis matches your description to the closest blueprint it knows — shop,
project tracker, business system, marketplace, or a general application. Being
more specific about the things in your business gets you closer.

---

## What Genesis does not do

Stated plainly, so nothing is a surprise:

- **It does not host your site.** You get the code; putting it online is a
  separate step (the `README.md` explains how).
- **It does not design your brand.** The interface is clean and functional, not
  bespoke.
- **The generated project needs PostgreSQL to run.** Genesis itself does not.
- **It builds foundations, not finished businesses.** Payments, email and
  similar integrations are scaffolded, not connected to real accounts.
