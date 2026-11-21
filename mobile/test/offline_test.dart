import 'dart:io';

import 'package:cloudbox/api.dart';
import 'package:cloudbox/offline.dart';
import 'package:flutter_test/flutter_test.dart';

Node node({String id = 'one', int size = 3, bool encrypted = false}) => Node(
  id: id,
  name: '../photo.txt',
  isDir: false,
  size: size,
  mime: 'text/plain',
  updatedAt: DateTime.now(),
  e2e: encrypted,
);

void main() {
  late Directory root;
  late OfflineStore store;
  setUp(() async {
    root = await Directory.systemTemp.createTemp('cloudbox-offline-test-');
    store = OfflineStore(Directory('${root.path}/account'));
  });
  tearDown(() async {
    await root.delete(recursive: true);
  });

  test('complete download survives reopening and can be removed', () async {
    await store.save(node(), Stream.value([1, 2, 3]));
    final saved = await OfflineStore(store.directory).list();
    expect(saved.single.name, '../photo.txt');
    expect(File(saved.single.path).parent.path, store.directory.path);
    expect(await File(saved.single.path).readAsBytes(), [1, 2, 3]);
    await store.remove('one');
    expect(await store.list(), isEmpty);
  });

  test('failed refresh preserves the previously saved copy', () async {
    await store.save(node(), Stream.value([1, 2, 3]));
    await expectLater(
      store.save(node(), Stream.value([4])),
      throwsA(isA<FileSystemException>()),
    );
    final saved = await store.list();
    expect(await File(saved.single.path).readAsBytes(), [1, 2, 3]);
    await expectLater(
      store.save(node(), Stream.error(const SocketException('offline'))),
      throwsA(isA<SocketException>()),
    );
    expect(await store.list(), hasLength(1));
  });

  test('refresh replaces the copy; logout clears all offline data', () async {
    await store.save(node(), Stream.value([1, 2, 3]));
    await store.save(node(size: 2), Stream.value([8, 9]));
    final saved = await store.list();
    expect(saved, hasLength(1));
    expect(await File(saved.single.path).readAsBytes(), [8, 9]);
    expect(await store.directory.list().length, 2);
    await store.clear();
    expect(await store.list(), isEmpty);
  });

  test('account and server separation, canonical server URL', () {
    final a = OfflineStore.accountKey('https://one/', 'alice');
    expect(a, OfflineStore.accountKey('https://one', 'Alice'));
    expect(a, isNot(OfflineStore.accountKey('https://two', 'alice')));
    expect(a, isNot(OfflineStore.accountKey('https://one', 'bob')));
  });

  test('encrypted files are never saved as ordinary downloads', () async {
    await expectLater(
      store.save(node(encrypted: true), Stream.value([1, 2, 3])),
      throwsArgumentError,
    );
    expect(await store.list(), isEmpty);
  });
}
