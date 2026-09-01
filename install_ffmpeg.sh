#!/usr/bin/env bash
echo 'WARNING: downloading and executing lpms/install_ffmpeg.sh, use it directly in case of issues'
curl https://raw.githubusercontent.com/Livepeer-FrameWorks/lpms/966922d9d66128a287bc1ce09b5e69c22495b275/install_ffmpeg.sh | bash -s $1
