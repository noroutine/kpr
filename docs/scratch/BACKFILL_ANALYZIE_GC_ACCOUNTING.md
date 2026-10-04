728edd160941:/app$ kpr registry analyze
catalog: 600 repos, 17056 tags, 9 sentinels
store  : 600 repos, 17055 tags, 8 sentinels, store status: paired
store Δ: +0 repos, -1 tag, -1 sentinel
fs     : 600 repos, 17056 tags, 9 sentinels
fs Δ   : +0 repos, +0 tags, +0 sentinels
revs   : 24999 revisions, 7943 untagged
blobs  : 55089 blobs, 101179 layer links, 1 upload
size   : 632.18 GiB blobs

gc before sweep

f45ad84e69ea:/app$ kpr registry analyze
catalog: 600 repos, 17057 tags, 10 sentinels
store  : 600 repos, 17056 tags, 9 sentinels, store status: paired
store Δ: +0 repos, -1 tag, -1 sentinel
fs     : 600 repos, 17057 tags, 10 sentinels
fs Δ   : +0 repos, +0 tags, +0 sentinels
revs   : 25000 revisions, 7943 untagged
blobs  : 55055 blobs, 101144 layer links, 5 uploads
size   : 632.13 GiB blobs


sweep

time kpr sweep --no-dry-run

...
time=2026-10-04T01:17:41.410Z level=INFO msg="sweep row" pass_id=1791076628799692761 repo=syncthing/syncthing tag=2.0.11 reason="keep-n:exceeds 10" outcome=deleted
time=2026-10-04T01:17:41.410Z level=INFO msg="sweep pass" pass_id=1791076628799692761 trigger=sweep dry_run=false performed=14066 planned=0 failed=0 untracked=0 skipped=false
sweep 1791076628799692761: 14066 performed, 0 planned, 0 failed, 0 untracked

real	0m32.618s
user	0m3.836s
sys	0m3.747s



