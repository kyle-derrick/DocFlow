import zipfile
z = zipfile.ZipFile('/tmp/webpkg.zip', 'w')
z.write('/tmp/webpkg-src/index.html', 'index.html')
z.close()
print('zip written')
