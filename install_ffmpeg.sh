#!/usr/bin/env bash
echo 'WARNING: downloading and executing lpms/install_ffmpeg.sh, use it directly in case of issues'
curl https://raw.githubusercontent.com/Livepeer-FrameWorks/lpms/158f43b2e8038672e1dbfd9e7eaf8691978ced24/install_ffmpeg.sh | bash -s $1
