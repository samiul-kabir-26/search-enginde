# Dataset attribution

Product listings and images used for development and evaluation come from
**Amazon Berkeley Objects (ABO)**: https://amazon-berkeley-objects.s3.amazonaws.com/index.html

> Collins, J., Goel, S., Deng, K., Luthra, A., Xu, L., Gundogdu, E., Zhang, X.,
> Yago Vicente, T. F., Dideriksen, T., Arora, H., Guillaumin, M., Malik, J.
> "ABO: Dataset and Benchmarks for Real-World 3D Object Understanding." CVPR 2022.

Data credit: **Amazon.com**. Dataset, archives and benchmarks: Matthieu Guillaumin, Thomas Dideriksen,
Kenan Deng, Himanshu Arora (Amazon.com), Jasmine Collins and Jitendra Malik (UC Berkeley).

License: **CC BY 4.0** (https://creativecommons.org/licenses/by/4.0/). Changes made: a 10K subset is
selected, titles are normalized, and images are re-hosted in S3 (SeaweedFS in dev). ABO data is used for the demo
tenant, development and evaluation only, never as production tenant data.
Prices, stock, vendors, users and events in `data/` are synthetic (docs/DATA.md §4–5).
