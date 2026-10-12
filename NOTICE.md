This repository builds and instruments these upstream implementations:

* enbility/eebus-go, enbility/ship-go and enbility/spine-go: MIT.
* NIBEGroup/openeebus: Apache License 2.0.
* OpenMUC jEEBUS controllable-system modules: Eclipse Public License 2.0.
* warmcat/libwebsockets: MIT, with additional notices for included sources.

The exact source revisions and archive checksums are in recipes/lock.json.
Original root license/notice files are copied to /licenses in each node image.
Upstream source headers are retained in generated trees. Generated overlays,
source archives, dependency locks and image provenance identify the modified
sources used to build a node. Java artifacts and OS packages retain their own
licenses; consult their bundled notices when redistributing an image.

The EEBUS specification documents used during development are not redistributed.
No license is selected for the original arena code yet.
