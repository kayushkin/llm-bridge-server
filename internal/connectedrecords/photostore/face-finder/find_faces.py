"""Find the faces on photo-store's thumbnails and post them to photo-store.

It measures and decides nothing: for each photo the model has not scanned,
it fetches the thumbnail, finds every face with InsightFace's buffalo_l
models (detection and a 512-number description of each face), and posts
what it found to POST /face-scans, including a scan that found no face.
Matching faces to people is photo-store's matcher's job, in Go.

    python -I find_faces.py --store-url http://127.0.0.1:8329 --worker 0 --workers 3

--recheck FILE scans again the photo ids listed in FILE, one per line, and
replaces a photo's faces only when the new scan differs from the stored one:
another number of faces, or a box that moved (overlap under --same-box). A
photo whose thumbnail changed after its scan needs this; an import that
changes a thumbnail now retires its faces itself.

Workers split the photos by id number, so several can run at once without
scanning the same photo twice. The models download to ~/.insightface on the
first run. buffalo_l is licensed for non-commercial use only.
"""

import argparse
import base64
import json
import sys
import time
import urllib.error
import urllib.request

import cv2
import numpy as np
from insightface.app import FaceAnalysis

MODEL = "insightface-buffalo_l"


def call(store_url, method, path, body=None):
    """One request to photo-store. A refused connection is retried for up to
    two minutes, since a deploy restarts the store; anything else is final."""
    data = None if body is None else json.dumps(body).encode()
    for attempt in range(25):
        request = urllib.request.Request(store_url + path, data=data, method=method,
                                         headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=120) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()
        except urllib.error.URLError as error:
            if attempt == 24:
                raise
            print(f"photo-store unreachable ({error.reason}); retrying in 5 s", file=sys.stderr, flush=True)
            time.sleep(5)


def find(analysis, image):
    """Every face on one decoded thumbnail, as POST /face-scans takes them."""
    height, width = image.shape[:2]
    faces = []
    for face in analysis.get(image):
        left, top, right, bottom = (float(v) for v in face.bbox)
        faces.append({
            "box": {"left": left / width, "top": top / height, "right": right / width, "bottom": bottom / height},
            "detection_score": float(face.det_score),
            "embedding_float32_base64": base64.b64encode(np.asarray(face.embedding, dtype="<f4").tobytes()).decode(),
        })
    return faces


def overlap(a, b):
    """Intersection over union of two boxes."""
    width = min(a["right"], b["right"]) - max(a["left"], b["left"])
    height = min(a["bottom"], b["bottom"]) - max(a["top"], b["top"])
    if width <= 0 or height <= 0:
        return 0.0
    shared = width * height
    area = lambda box: (box["right"] - box["left"]) * (box["bottom"] - box["top"])
    return shared / (area(a) + area(b) - shared)


def same_faces(stored, found, least_overlap):
    """Whether a new scan found the faces already stored: as many, each new box
    overlapping a different stored one by at least least_overlap."""
    if len(stored) != len(found):
        return False
    unused = list(stored)
    for face in found:
        best = max(unused, key=lambda kept: overlap(kept["box"], face["box"]), default=None)
        if best is None or overlap(best["box"], face["box"]) < least_overlap:
            return False
        unused.remove(best)
    return True


def recheck(arguments, analysis):
    with open(arguments.recheck) as listing:
        photo_ids = [line.strip() for line in listing if line.strip()]
    mine = [photo_id for photo_id in photo_ids if int(photo_id.split("_")[1]) % arguments.workers == arguments.worker]
    kept = replaced = 0
    for photo_id in mine:
        status, thumbnail = call(arguments.store_url, "GET", f"/photos/{photo_id}/thumbnail")
        image = cv2.imdecode(np.frombuffer(thumbnail, np.uint8), cv2.IMREAD_COLOR) if status == 200 else None
        if image is None:
            sys.exit(f"{photo_id}: no readable thumbnail ({status})")
        status, body = call(arguments.store_url, "GET", f"/faces?photo_id={photo_id}&model={MODEL}")
        if status != 200:
            sys.exit(f"{photo_id}: GET /faces {status} {body[:300]!r}")
        stored = json.loads(body)
        found = find(analysis, image)
        if same_faces(stored, found, arguments.same_box):
            kept += 1
        else:
            status, answer = call(arguments.store_url, "POST", "/face-scans",
                                  {"photo_id": photo_id, "model": MODEL, "faces": found, "replaces": True})
            if status != 201:
                sys.exit(f"{photo_id}: POST /face-scans {status} {answer[:300]!r}")
            replaced += 1
            print(f"{photo_id}: {len(stored)} stored faces replaced by {len(found)}", flush=True)
        if (kept + replaced) % 100 == 0:
            print(f"worker {arguments.worker}: {kept + replaced} of {len(mine)} rechecked; {replaced} replaced", flush=True)
    print(f"worker {arguments.worker}: done; {kept} photos kept their faces, {replaced} had them replaced", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--store-url", required=True)
    parser.add_argument("--worker", type=int, default=0)
    parser.add_argument("--workers", type=int, default=1)
    parser.add_argument("--threads", type=int, default=2, help="CPU threads for the model")
    parser.add_argument("--limit", type=int, default=0, help="stop after this many photos; 0 = all")
    parser.add_argument("--recheck", help="a file of photo ids to scan again, replacing faces that moved")
    parser.add_argument("--same-box", type=float, default=0.7,
                        help="with --recheck, the least overlap (intersection over union) at which a stored box counts as the same face")
    parser.add_argument("--photo", action="append", default=[], help="scan only this photo id, ahead of the queue; repeatable")
    arguments = parser.parse_args()

    import onnxruntime
    options = onnxruntime.SessionOptions()
    options.intra_op_num_threads = arguments.threads
    analysis = FaceAnalysis(name="buffalo_l", providers=["CPUExecutionProvider"],
                            allowed_modules=["detection", "recognition"], session_options=options)
    analysis.prepare(ctx_id=-1, det_size=(640, 640))

    if arguments.recheck:
        recheck(arguments, analysis)
        return

    done = 0
    skipped = set()
    named_scanned = False
    while True:
        if arguments.photo:
            if named_scanned:
                break
            mine, named_scanned = arguments.photo, True
        else:
            status, body = call(arguments.store_url, "GET", f"/face-scans/pending?model={MODEL}&limit=1000")
            if status != 200:
                sys.exit(f"pending: {status} {body[:300]!r}")
            mine = [photo_id for photo_id in json.loads(body)
                    if int(photo_id.split("_")[1]) % arguments.workers == arguments.worker and photo_id not in skipped]
        if not mine:
            break
        for photo_id in mine:
            status, thumbnail = call(arguments.store_url, "GET", f"/photos/{photo_id}/thumbnail")
            image = cv2.imdecode(np.frombuffer(thumbnail, np.uint8), cv2.IMREAD_COLOR) if status == 200 else None
            if image is None:
                print(f"{photo_id}: no readable thumbnail ({status}); skipped", file=sys.stderr, flush=True)
                skipped.add(photo_id)
                continue
            faces = find(analysis, image)
            status, answer = call(arguments.store_url, "POST", "/face-scans", {"photo_id": photo_id, "model": MODEL, "faces": faces})
            if status == 409:
                # Another finder scanned it meanwhile; its faces stand.
                skipped.add(photo_id)
                continue
            if status != 201:
                sys.exit(f"{photo_id}: POST /face-scans {status} {answer[:300]!r}")
            done += 1
            if done % 100 == 0:
                print(f"worker {arguments.worker}: {done} photos scanned", flush=True)
            if arguments.limit and done >= arguments.limit:
                return
    print(f"worker {arguments.worker}: done, {done} photos scanned, {len(skipped)} skipped", flush=True)


if __name__ == "__main__":
    main()
