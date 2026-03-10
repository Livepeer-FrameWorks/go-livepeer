#!/usr/bin/env bash
echo 'WARNING: downloading and executing lpms/install_ffmpeg.sh, use it directly in case of issues'
curl https://raw.githubusercontent.com/Livepeer-FrameWorks/lpms/1eba2fe2dcea85009480ca4bf61448e892a48ab9/install_ffmpeg.sh | bash -s $1
