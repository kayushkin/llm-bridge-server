# possession-store

An inventory of the things a person owns: clothes, computers, audio gear,
instruments, vehicles and everything else, with photos, prices and where each
thing is kept.

- **Kinds.** Every possession has the same core record. Clothes, computers,
  computer parts, audio devices, instruments and vehicles also get a table of
  their own for the facts only that kind has. Each kind has its own id prefix.
- **Outfits** are sets of garments. A model can suggest them from the
  wardrobe, the weather and the occasion, and the owner keeps or rejects them.
- **Photo review.** Two models read each photo of a room on their own, then
  see each other's answers and settle on one list of what is in it. Where
  they still differ, or cannot tell a new thing from one already recorded,
  the store asks the owner a question.
- **Purchases** from Amazon, Facebook Marketplace and receipts are kept line
  by line. A line becomes a possession once someone confirms it is one.

`CONTRACT.md` is the API. `deploy.sh` tests, builds and installs the service
and `possession-add-photos`, which brings photos in and queues them for review.
